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

package build

import (
	"fmt"
	"path"
	"strings"

	"github.com/alauda-mesh/release-builder/pkg/model"
	"github.com/alauda-mesh/release-builder/pkg/util"
)

// Rpm produces an rpm package just for the sidecar
func Rpm(manifest model.Manifest) error {
	for _, plat := range manifest.Architectures {
		_, arch, _ := strings.Cut(plat, "/")
		envs := []string{"TARGET_ARCH=" + arch}
		output := "istio-sidecar.rpm"
		if arch != "amd64" {
			output = fmt.Sprintf("istio-sidecar-%s.rpm", arch)
		}

		if err := runRpm(manifest, envs, arch, output); err != nil {
			return fmt.Errorf("failed to run rpm for arch %s: %v", arch, err)
		}
	}
	return nil
}

func runRpm(manifest model.Manifest, envs []string, arch, output string) error {
	if err := util.RunMake(manifest, "istio", envs, "rpm/fpm"); err != nil {
		return fmt.Errorf("failed to build sidecar.rpm: %v", err)
	}
	if err := util.CopyFile(path.Join(manifest.RepoArchOutDir("istio", arch), "istio-sidecar.rpm"), path.Join(manifest.OutDir(), "rpm", output)); err != nil {
		return fmt.Errorf("failed to package istio-sidecar.rpm: %v", err)
	}
	if err := util.CreateSha(path.Join(manifest.OutDir(), "rpm", output)); err != nil {
		return fmt.Errorf("failed to package istio-sidecar.rpm: %v", err)
	}
	return nil
}
