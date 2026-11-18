package divide

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4/internal/mp4test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDivideHEVC(t *testing.T) {
	bin := mp4test.BuildHEVC(mp4test.HEVCOptions{
		SampleEntry: "hvc1",
		WithHvcC:    true,
		Fragmented:  true,
	})

	input := filepath.Join(t.TempDir(), "input.mp4")
	require.NoError(t, os.WriteFile(input, bin, 0644))
	outputDir := t.TempDir()

	require.NoError(t, divide(input, outputDir))

	playlist, err := os.ReadFile(filepath.Join(outputDir, playlistFileName))
	require.NoError(t, err)
	assert.Contains(t, string(playlist), `CODECS="hvc1.1.6.L120.90"`)
	assert.Contains(t, string(playlist), "RESOLUTION=320x180")
	assert.NotContains(t, string(playlist), "avc1")

	_, err = os.Stat(filepath.Join(outputDir, videoDirName, initMP4FileName))
	assert.NoError(t, err)
}

func TestDivideEncryptedHEVC(t *testing.T) {
	bin := mp4test.BuildHEVC(mp4test.HEVCOptions{
		SampleEntry:    "encv",
		OriginalFormat: "hvc1",
		WithHvcC:       true,
		Fragmented:     true,
	})

	input := filepath.Join(t.TempDir(), "input.mp4")
	require.NoError(t, os.WriteFile(input, bin, 0644))
	outputDir := t.TempDir()

	require.NoError(t, divide(input, outputDir))

	playlist, err := os.ReadFile(filepath.Join(outputDir, playlistFileName))
	require.NoError(t, err)
	assert.Contains(t, string(playlist), `CODECS="hvc1.1.6.L120.90"`)
	_, err = os.Stat(filepath.Join(outputDir, encVideoDirName, initMP4FileName))
	assert.NoError(t, err)
}

func TestDivideAVCCompatibility(t *testing.T) {
	// The existing AVC + AAC fixture must keep producing the codec strings that
	// are probed from its sample entries.
	bin, err := os.ReadFile("../../../../testdata/sample_fragmented.mp4")
	require.NoError(t, err)

	input := filepath.Join(t.TempDir(), "input.mp4")
	require.NoError(t, os.WriteFile(input, bin, 0644))
	outputDir := t.TempDir()

	require.NoError(t, divide(input, outputDir))

	playlist, err := os.ReadFile(filepath.Join(outputDir, playlistFileName))
	require.NoError(t, err)
	assert.Contains(t, string(playlist), `CODECS="avc1.4D401F,mp4a.40.2"`)
	assert.Contains(t, string(playlist), "RESOLUTION=1280x720")
	assert.Contains(t, string(playlist), `CHANNELS="2"`)
}
