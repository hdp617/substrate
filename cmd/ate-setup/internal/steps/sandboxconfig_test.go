// Copyright 2026 Google LLC
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

package steps

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/config"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/render"
	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

// placeholderPattern matches the ${NAME} form the manifests use.
var placeholderPattern = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// substitutedPlaceholders are the placeholders DeployAteSystem fills in, by way
// of SubstituteVersion and the render.Expand over the resolved bundle.
var substitutedPlaceholders = map[string]bool{
	"SUBSTRATE_VERSION":        true,
	"SUBSTRATE_VERSION_SUFFIX": true,
	"BUCKET_NAME":              true,
}

// TestAteInstallManifestPlaceholders guards the manifests the install sweeps:
// off the kind overlay DeployAteSystem concatenates every YAML directly under
// manifests/ate-install, so one carrying a placeholder nobody substitutes
// reaches the cluster with the literal ${...} in it rather than failing the
// install. Subdirectories are excluded for the same reason the sweep excludes
// them: they only ship through an overlay that names them.
func TestAteInstallManifestPlaceholders(t *testing.T) {
	cfg := &config.Config{Root: repoRoot(t)}
	entries, err := os.ReadDir(cfg.Manifest())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var checked int
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		checked++
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := os.ReadFile(cfg.Manifest(entry.Name()))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			for _, match := range placeholderPattern.FindAllStringSubmatch(string(data), -1) {
				if !substitutedPlaceholders[match[1]] {
					t.Errorf("manifest carries %s, which nothing in DeployAteSystem substitutes", match[0])
				}
			}
		})
	}
	if checked == 0 {
		t.Fatalf("no manifests found in %s", cfg.Manifest())
	}
}

// TestMicrovmSandboxConfigRenders covers the one placeholder-bearing
// SandboxConfig: the bucket has to land in every asset URL, since an
// unsubstituted one would be accepted by the API server and only fail later,
// when atelet tried to fetch from a bucket literally named ${BUCKET_NAME}.
func TestMicrovmSandboxConfigRenders(t *testing.T) {
	cfg := &config.Config{Root: repoRoot(t)}
	rendered, err := render.Template(cfg.Manifest("sandboxconfig-microvm.yaml"),
		map[string]string{"BUCKET_NAME": "test-bucket"}, nil)
	if err != nil {
		t.Fatalf("render.Template: %v", err)
	}
	if loc := placeholderPattern.FindString(string(rendered)); loc != "" {
		t.Errorf("unsubstituted placeholder remains: %s", loc)
	}

	var sandboxConfig v1alpha1.SandboxConfig
	if err := yaml.Unmarshal(rendered, &sandboxConfig); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if sandboxConfig.Name != "microvm" {
		t.Errorf("SandboxConfig is named %q, want microvm: ActorTemplates reference it by that name", sandboxConfig.Name)
	}
	if sandboxConfig.Spec.SandboxClass != v1alpha1.SandboxClassMicroVM {
		t.Errorf("sandboxClass is %q, want %s", sandboxConfig.Spec.SandboxClass, v1alpha1.SandboxClassMicroVM)
	}
	// Both architectures, and every asset each needs to boot a micro-VM.
	for _, arch := range []string{"amd64", "arm64"} {
		assets, ok := sandboxConfig.Spec.Assets[arch]
		if !ok {
			t.Errorf("no assets for %s; a node of that architecture cannot start a micro-VM actor", arch)
			continue
		}
		for _, name := range []string{"cloud-hypervisor", "virtiofsd", "kata-kernel", "kata-image"} {
			asset, ok := assets[name]
			if !ok {
				t.Errorf("%s assets are missing %s", arch, name)
				continue
			}
			if want := "gs://test-bucket/"; !strings.HasPrefix(asset.URL, want) {
				t.Errorf("%s/%s url is %q, want it under %s", arch, name, asset.URL, want)
			}
			if asset.SHA256 == "" {
				t.Errorf("%s/%s has no sha256; atelet would have nothing to verify the download against", arch, name)
			}
		}
	}
}
