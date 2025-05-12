// Copyright Istio Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package publish

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"istio.io/istio/pkg/log"

	"github.com/alauda-mesh/release-builder/pkg/model"
)

func NewS3Client(ctx context.Context) (*s3.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	s3Client := s3.NewFromConfig(cfg)
	return s3Client, nil
}

// S3Archive publishes the final release archive to the given GCS bucket
func S3Archive(manifest model.Manifest, bucket string, aliases []string) error {
	ctx := context.Background()
	client, err := NewS3Client(ctx)
	if err != nil {
		// TODO: Handle error.
		return err
	}

	// Allow the caller to pass a reference like bucket/folder/subfolder, but split this to
	// bucket, and folder/subfolder prefix
	splitbucket := strings.SplitN(bucket, "/", 2)
	bucketName := splitbucket[0]
	objectPrefix := ""
	if len(splitbucket) > 1 {
		objectPrefix = splitbucket[1]
	}
	if err := filepath.Walk(manifest.Directory, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		objName := path.Join(objectPrefix, manifest.Version, strings.TrimPrefix(p, manifest.Directory))

		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("failed to open %v: %v", p, err)
		}
		defer f.Close()

		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objName),
			Body:   bufio.NewReader(f),
		})
		if err != nil {
			return fmt.Errorf("failed to put object: %v", err)
		}

		log.Infof("Wrote %v to s3://%s/%s", p, bucketName, objName)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to walk directory: %v", err)
	}

	// Add alias objects. These are basically symlinks/tags for GCS, pointing to the latest version
	for _, alias := range aliases {
		objName := path.Join(objectPrefix, alias)
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objName),
			Body:   strings.NewReader(manifest.Version),
		})
		if err != nil {
			return fmt.Errorf("failed to write alias %v: %v", alias, err)
		}

		log.Infof("Wrote %v to s3://%s/%s", alias, bucketName, path.Join(objectPrefix, alias))
	}

	return nil
}

func FetchObject(client *s3.Client, bucket string, objectPrefix string, filename string) ([]byte, error) {
	objName := filepath.Join(objectPrefix, filename)
	getObjectResult, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objName),
	})
	if err != nil {
		return nil, err
	}
	defer getObjectResult.Body.Close()
	c, err := io.ReadAll(getObjectResult.Body)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// MutateObject allows pulling a file from GCS, mutating it, then pushing it back up. This adds checks
// to ensure that if the file is mutated in the meantime, the process is repeated.
func MutateObject(outDir string, client *s3.Client, bucket string, objectPrefix string, filename string, f func() error) error {
	for i := 0; i < 10; i++ {
		err := mutateObjectInner(outDir, client, bucket, objectPrefix, filename, f)
		if err == ErrIndexOutOfDate {
			log.Warnf("Write conflict, trying again")
			continue
		}
		return err
	}
	return fmt.Errorf("max conflicts attempted")
}

func mutateObjectInner(outDir string, client *s3.Client, bucket string, objectPrefix string, filename string, f func() error) error {
	objName := filepath.Join(objectPrefix, filename)
	outFile := filepath.Join(outDir, filename)
	objResult, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objName),
	})
	if err != nil {
		var notFoundErr *types.NotFound
		if errors.As(err, &notFoundErr) {
			// Missing is fine
			log.Warnf("existing file %v does not exist", filename)
		} else {
			return fmt.Errorf("failed to fetch attributes: %v", err)
		}
	}
	etag := ""
	if objResult != nil {
		etag = aws.ToString(objResult.ETag)
		log.Infof("Object %v currently has etag", objName, etag)

		defer objResult.Body.Close()
		idx, err := io.ReadAll(objResult.Body)
		if err != nil {
			return err
		}
		if err := os.WriteFile(outFile, idx, 0o644); err != nil {
			return err
		}
		log.Infof("Wrote %v", outFile)
	}

	// Run our action
	if err := f(); err != nil {
		return fmt.Errorf("action failed: %v", err)
	}

	// Now we want to (try to) write it
	res, err := os.Open(outFile)
	if err != nil {
		return fmt.Errorf("failed to open %v: %v", res.Name(), err)
	}
	defer res.Close()

	pubObjectInput := &s3.PutObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(objName),
		Body:         bufio.NewReader(res),
		CacheControl: aws.String("no-cache, max-age=0, no-transform"),
		ContentType:  aws.String("text/yaml"),
	}
	if etag != "" {
		pubObjectInput.IfMatch = aws.String(etag)
	}

	_, err = client.PutObject(context.Background(), pubObjectInput)
	if err != nil {
		return fmt.Errorf("failed writing %v: %v", res.Name(), err)
	}
	return nil
}

var ErrIndexOutOfDate = errors.New("index is out-of-date")
