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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"istio.io/istio/pkg/log"

	"github.com/alauda-mesh/release-builder/pkg/model"
)

func NewS3Client(ctx context.Context) (*minio.Client, error) {
	endpoint := "https://s3.amazonaws.com"
	if ep := os.Getenv("S3_ENDPOINT"); ep != "" {
		endpoint = ep
	}

	useSSL := strings.HasPrefix(endpoint, "https://")
	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewEnvAWS(),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}

	return minioClient, nil
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

		_, err = client.FPutObject(ctx, bucketName, objName, p, minio.PutObjectOptions{})
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
		_, err = client.PutObject(ctx, bucketName, objName,
			strings.NewReader(manifest.Version), int64(len(manifest.Version)),
			minio.PutObjectOptions{})
		if err != nil {
			return fmt.Errorf("failed to write alias %v: %v", alias, err)
		}

		log.Infof("Wrote %v to s3://%s/%s", alias, bucketName, path.Join(objectPrefix, alias))
	}

	return nil
}

func FetchObject(client *minio.Client, bucket string, objectPrefix string, filename string) ([]byte, error) {
	objName := filepath.Join(objectPrefix, filename)
	getObjectResult, err := client.GetObject(context.Background(), bucket, objName, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer getObjectResult.Close()
	c, err := io.ReadAll(getObjectResult)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// MutateObject allows pulling a file from GCS, mutating it, then pushing it back up. This adds checks
// to ensure that if the file is mutated in the meantime, the process is repeated.
func MutateObject(outDir string, client *minio.Client, bucket string, objectPrefix string, filename string, f func() error) error {
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

func mutateObjectInner(outDir string, client *minio.Client, bucket string, objectPrefix string, filename string, f func() error) error {
	objName := filepath.Join(objectPrefix, filename)
	outFile := filepath.Join(outDir, filename)
	objResult, err := client.GetObject(context.Background(), bucket, objName, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			// Missing is fine
			log.Warnf("existing file %v does not exist", filename)
		} else {
			return fmt.Errorf("failed to fetch attributes: %v", err)
		}
	}
	etag := ""
	if objResult != nil {
		defer objResult.Close()
		idx, err := io.ReadAll(objResult)
		if err != nil {
			return err
		}
		if err := os.WriteFile(outFile, idx, 0o644); err != nil {
			return err
		}
		log.Infof("Wrote %v", outFile)

		objInfo, _ := objResult.Stat()
		etag = objInfo.ETag
		log.Infof("Object %v currently has etag: %s", objName, etag)
	}

	// Run our action
	if err := f(); err != nil {
		return fmt.Errorf("action failed: %v", err)
	}

	// Now we want to (try to) write it
	pubObjectOptions := minio.PutObjectOptions{
		CacheControl: "no-cache, max-age=0, no-transform",
		ContentType:  "text/yaml",
	}
	if etag != "" {
		pubObjectOptions.SetMatchETag(etag)
	}

	_, err = client.FPutObject(context.Background(), bucket, objName, outFile, pubObjectOptions)
	if err != nil {
		return fmt.Errorf("failed writing %v: %v", outFile, err)
	}
	return nil
}

var ErrIndexOutOfDate = errors.New("index is out-of-date")
