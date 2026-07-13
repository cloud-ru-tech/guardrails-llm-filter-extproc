package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/config"
)

func TestLoadDefaultPaths(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"/v1/chat/completions": "chat_completions",
		"/v1/messages":         "messages",
		"/v1/responses":        "responses",
	}, cfg.Guardrails.Paths)
}

func TestLoadDefaultDataTypesIncludesCustom(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)
	// 6 (CUSTOM) must be enabled by default, otherwise custom rules created via
	// the API silently never scan.
	assert.Equal(t, "1,2,3,4,5,6", cfg.Guardrails.DataTypes)
}

func TestLoadPathsOverrideMergesOverDefaults(t *testing.T) {
	t.Setenv("GUARDRAILS_PATHS", "/llm/chat:chat_completions")

	cfg, err := config.Load()
	require.NoError(t, err)
	// A partial override must MERGE on top of the built-in core paths so it
	// can never silently disable masking for a core endpoint.
	assert.Equal(t, map[string]string{
		"/llm/chat":            "chat_completions",
		"/v1/chat/completions": "chat_completions",
		"/v1/messages":         "messages",
		"/v1/responses":        "responses",
	}, cfg.Guardrails.Paths)
}

func TestLoadPathsOverrideForCorePathWins(t *testing.T) {
	// A user entry for a core path overrides the default format; the other
	// core paths are still merged in.
	t.Setenv("GUARDRAILS_PATHS", "/v1/messages:chat_completions")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "chat_completions", cfg.Guardrails.Paths["/v1/messages"])
	assert.Equal(t, "chat_completions", cfg.Guardrails.Paths["/v1/chat/completions"])
	assert.Len(t, cfg.Guardrails.Paths, 3)
}

func TestLoadPathsInvalidFormatFailsBoot(t *testing.T) {
	t.Setenv("GUARDRAILS_PATHS", "/v1/chat:not-a-format")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GUARDRAILS_PATHS")
}

func TestLoadPathsWithoutSlashFailsBoot(t *testing.T) {
	t.Setenv("GUARDRAILS_PATHS", "v1/chat:chat_completions")

	_, err := config.Load()
	require.Error(t, err)
}

func TestLoadNegativeMaskParallelMinBytesFailsBoot(t *testing.T) {
	t.Setenv("GUARDRAILS_MASK_PARALLEL_MIN_BYTES", "-1")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GUARDRAILS_MASK_PARALLEL_MIN_BYTES")
}

func TestLoadZeroMaskParallelMinBytesUsesDefault(t *testing.T) {
	t.Setenv("GUARDRAILS_MASK_PARALLEL_MIN_BYTES", "0")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.Guardrails.MaskParallelMinBytes)
}
