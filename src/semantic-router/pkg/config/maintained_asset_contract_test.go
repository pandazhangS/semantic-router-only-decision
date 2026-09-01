package config

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// The decision-only fork keeps only the config/recipes catalog from the
// maintained asset set; the deploy/, e2e/, and bench/ trees (and their
// contract lists) were removed with the trees themselves.

var maintainedRecipeFiles = []string{
	"README.md",
	"config.yaml",
	"metadata.yaml",
	"probes.yaml",
	"recipe.dsl",
}

const builtInRecipeCatalogDirectory = "built-in"

func TestMaintainedRecipeDirectoriesAreCompleteAndSymmetric(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "config", "recipes")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read recipe catalog: %v", err)
	}

	var actualDirectories []string
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() == builtInRecipeCatalogDirectory {
				continue
			}
			actualDirectories = append(actualDirectories, entry.Name())
			continue
		}
		if entry.Name() != "README.md" && entry.Name() != "CONFORMANCE.md" {
			t.Errorf("recipe catalog root contains non-catalog file %q", entry.Name())
		}
	}
	sort.Strings(actualDirectories)
	if len(actualDirectories) == 0 {
		t.Fatal("recipe catalog must contain at least one maintained recipe")
	}

	for _, name := range actualDirectories {
		t.Run(name, func(t *testing.T) {
			assertRecipeDirectoryContract(t, root, name)
		})
	}
}

func assertRecipeDirectoryContract(t *testing.T, root, name string) {
	t.Helper()
	directory := filepath.Join(root, name)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() == ".vllm-sr" {
				continue
			}
			t.Fatalf("%s contains unexpected nested directory %q", directory, entry.Name())
		}
		actual = append(actual, entry.Name())
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, maintainedRecipeFiles) {
		t.Fatalf("%s files = %v, want exactly %v", directory, actual, maintainedRecipeFiles)
	}

	configRel := filepath.ToSlash(filepath.Join("config", "recipes", name, "config.yaml"))
	validateMaintainedConfigAsset(t, configRel, readMaintainedConfigAsset(t, configRel))
}

func readMaintainedConfigAsset(t *testing.T, rel string) []byte {
	t.Helper()
	return mustReadRepoFile(t, rel)
}

func validateMaintainedConfigAsset(t *testing.T, rel string, data []byte) {
	t.Helper()
	raw := decodeYAMLMap(t, data, rel)
	assertNoLegacySteadyStateKeys(t, rel, raw)
	assertNoRemovedDomainPolicyKeys(t, rel, raw)
	if _, err := ParseYAMLBytes(data); err != nil {
		t.Fatalf("%s no longer parses as a maintained canonical config asset: %v", rel, err)
	}
}

// removedDomainPolicyKeys are per-domain policy keys from the pre-plugin config
// schema. #681 moved these behaviours onto decision plugins and reduced
// routing.signals.domains to metadata plus model scores, so the loader has
// ignored them ever since. A shipped asset that still sets one advertises a
// knob the router does not have.
var removedDomainPolicyKeys = []string{
	"system_prompt_enabled",
	"system_prompt_mode",
	"semantic_cache_enabled",
	"semantic_cache_similarity_threshold",
	"jailbreak_enabled",
	"jailbreak_threshold",
	"pii_enabled",
	"pii_threshold",
}

func assertNoRemovedDomainPolicyKeys(t *testing.T, rel string, raw map[string]interface{}) {
	t.Helper()
	for _, domain := range assetDomainEntries(raw) {
		name, _ := domain["name"].(string)
		for _, key := range removedDomainPolicyKeys {
			if _, ok := domain[key]; ok {
				t.Errorf("%s sets removed per-domain policy key %q on domain %q; the router ignores it, configure the matching decision plugin instead", rel, key, name)
			}
		}
	}
}

// assetDomainEntries returns routing.signals.domains, or nothing when the asset
// declares no domain signal.
func assetDomainEntries(raw map[string]interface{}) []map[string]interface{} {
	routing, ok := raw["routing"].(map[string]interface{})
	if !ok {
		return nil
	}
	signals, ok := routing["signals"].(map[string]interface{})
	if !ok {
		return nil
	}
	domains, ok := signals["domains"].([]interface{})
	if !ok {
		return nil
	}
	entries := make([]map[string]interface{}, 0, len(domains))
	for _, domain := range domains {
		if typed, ok := domain.(map[string]interface{}); ok {
			entries = append(entries, typed)
		}
	}
	return entries
}

func assertNoLegacySteadyStateKeys(t *testing.T, rel string, raw map[string]interface{}) {
	t.Helper()
	for _, key := range []string{
		"signals",
		"decisions",
		"keyword_rules",
		"embedding_rules",
		"categories",
		"fact_check_rules",
		"user_feedback_rules",
		"reask_rules",
		"preference_rules",
		"language_rules",
		"context_rules",
		"complexity_rules",
		"modality_rules",
		"role_bindings",
		"jailbreak",
		"pii",
		"default_model",
		"reasoning_families",
		"default_reasoning_effort",
		"model_config",
		"vllm_endpoints",
		"provider_profiles",
		"strategy",
		"bert_model",
	} {
		if _, ok := raw[key]; ok {
			t.Fatalf("%s still uses legacy top-level key %q; migrate it to canonical providers/routing/global", rel, key)
		}
	}
}

func mustReadRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", rel, err)
	}
	return data
}

func decodeYAMLMap(t *testing.T, data []byte, rel string) map[string]interface{} {
	t.Helper()
	var root map[string]interface{}
	if err := yamlv3.Unmarshal(data, &root); err != nil {
		t.Fatalf("failed to decode %s: %v", rel, err)
	}
	return root
}
