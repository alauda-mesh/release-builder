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
	"os"
	"path"
	"regexp"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"istio.io/istio/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/alauda-mesh/release-builder/pkg/model"
	"github.com/alauda-mesh/release-builder/pkg/util"
)

var (
	// Currently tags are set as `release-1.x-latest-daily` or `latest` or `1.x-dev`
	tagRegexes = []*regexp.Regexp{
		regexp.MustCompile(`tag: .*-latest-daily`),
		regexp.MustCompile(`tag: latest`),
		regexp.MustCompile(`tag: 1\..-dev`),
	}

	quotedTagRegexes = []*regexp.Regexp{
		regexp.MustCompile(`"tag": "latest"`),
	}

	// Currently tags are set as `gcr.io/istio-testing` or `gcr.io/istio-release`
	hubs = []string{"gcr.io/istio-testing", "gcr.io/istio-release"}

	// helmCharts contains all helm charts we will package and publish
	helmCharts = []string{
		"manifests/charts/base",
		"manifests/charts/gateway",
		"manifests/charts/gateways/istio-egress",
		"manifests/charts/gateways/istio-ingress",
		"manifests/charts/istio-cni",
		"manifests/charts/ztunnel",
		"manifests/charts/istio-control/istio-discovery",
		"manifests/sample-charts/ambient",
	}

	// repoHelmCharts contains the "core" subset of the above helm charts we will release to the helm repo.
	repoHelmCharts = []string{
		"manifests/charts/base",
		"manifests/charts/gateway",
		"manifests/charts/istio-cni",
		"manifests/charts/ztunnel",
		"manifests/charts/istio-control/istio-discovery",
	}

	// repoSampleHelmCharts contains all helm charts we will release/publish as samples, rather than
	// core charts
	repoSampleHelmCharts = []string{
		"manifests/sample-charts/ambient",
	}
)

// Similar to sanitizeChart, but works on generic templates rather than only Helm charts.
// This updates the hub and tag fields for a single file
func updateValues(manifest model.Manifest, p string) error {
	read, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	contents := string(read)

	// The hub and tag should be update
	for _, hub := range hubs {
		contents = strings.ReplaceAll(contents, fmt.Sprintf("hub: %s", hub), fmt.Sprintf("hub: %s", manifest.Docker))
		contents = strings.ReplaceAll(contents, fmt.Sprintf("\"hub\": \"%s\"", hub), fmt.Sprintf("\"hub\": \"%s\"", manifest.Docker))
	}
	for _, tagRegex := range tagRegexes {
		contents = tagRegex.ReplaceAllString(contents, fmt.Sprintf("tag: %s", manifest.Version))
	}

	for _, quotedTagRegex := range quotedTagRegexes {
		contents = quotedTagRegex.ReplaceAllString(contents, fmt.Sprintf("\"tag\": \"%s\"", manifest.Version))
	}

	err = os.WriteFile(p, []byte(contents), 0)
	if err != nil {
		return err
	}

	return nil
}

// SanitizeAllCharts rewrites versions, tags, and hubs for helm charts. This is done independent of Helm
// as it is required for both the helm charts and the archive
func SanitizeAllCharts(manifest model.Manifest) error {
	for _, chart := range helmCharts {
		if err := stampChartForRelease(manifest, path.Join(manifest.RepoDir("istio"), chart)); err != nil {
			return fmt.Errorf("failed to sanitize chart %v: %v", chart, err)
		}
	}
	return nil
}

// 1. Updates the chart versions to the release version
// 2. Updates values.yaml files with publishable defaults (hub/tag/etc)
func stampChartForRelease(manifest model.Manifest, s string) error {
	chartPath := path.Join(s, "Chart.yaml")
	currentVersion, err := os.ReadFile(chartPath)
	if err != nil {
		return err
	}

	chartFile := chart.Metadata{}
	if err := yaml.Unmarshal(currentVersion, &chartFile); err != nil {
		log.Errorf("unmarshal failed for Chart.yaml: %v", string(currentVersion))
		return fmt.Errorf("failed to unmarshal chart: %v", err)
	}

	// Update versions
	chartFile.Version = manifest.Version
	chartFile.AppVersion = manifest.Version

	// if chart has "file://" local/dev subchart dependencies, update with release version refs
	// note that we do not really need to update the repo refs to something other than `file://`,
	// as the full deps will be bundled in the `.tgz` either way.
	if len(chartFile.Dependencies) > 0 {
		for _, dep := range chartFile.Dependencies {
			if strings.Contains(dep.Repository, "file://") {
				dep.Version = manifest.Version
			}
		}
	}

	// Write updated chart.yaml back out
	contents, err := yaml.Marshal(chartFile)
	if err != nil {
		return err
	}

	err = os.WriteFile(chartPath, contents, 0)
	if err != nil {
		return err
	}

	if err := updateValues(manifest, path.Join(s, "values.yaml")); err != nil {
		return err
	}
	return nil
}

func HelmCharts(manifest model.Manifest) error {
	dst := path.Join(manifest.OutDir(), "helm")
	samplesDst := path.Join(dst, "samples")

	if err := os.MkdirAll(path.Join(dst), 0o750); err != nil {
		return fmt.Errorf("failed to make destination directory %v: %v", dst, err)
	}

	if err := os.MkdirAll(path.Join(samplesDst), 0o750); err != nil {
		return fmt.Errorf("failed to make destination directory %v: %v", dst, err)
	}

	for _, chart := range repoSampleHelmCharts {
		inDir := path.Join(manifest.RepoDir("istio"), chart)
		outDir := path.Join(manifest.WorkDir(), "charts", "samples", chart)

		if err := prepChartForPackaging(inDir, outDir); err != nil {
			return err
		}

		c := util.VerboseCommand("helm", "package", outDir)
		c.Dir = samplesDst
		if err := c.Run(); err != nil {
			return fmt.Errorf("package %v: %v", chart, err)
		}
	}

	for _, chart := range repoHelmCharts {
		inDir := path.Join(manifest.RepoDir("istio"), chart)
		outDir := path.Join(manifest.WorkDir(), "charts", chart)

		if err := prepChartForPackaging(inDir, outDir); err != nil {
			return err
		}

		c := util.VerboseCommand("helm", "package", outDir)
		c.Dir = dst
		if err := c.Run(); err != nil {
			return fmt.Errorf("package %v: %v", chart, err)
		}
	}
	return nil
}

func prepChartForPackaging(inDir, outDir string) error {
	// before copying, do dep update if needed
	// Helm will skip for us if the chart has no deps
	depCmd := util.VerboseCommand("helm", "dep", "update")
	depCmd.Dir = inDir
	if err := depCmd.Run(); err != nil {
		return fmt.Errorf("dep update %v: %v", inDir, err)
	}

	// Now the deps are updated/inlined, we can copy and package
	if err := util.CopyDir(inDir, outDir); err != nil {
		return err
	}

	return nil
}
