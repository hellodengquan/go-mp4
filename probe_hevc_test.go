package mp4_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/abema/go-mp4"
	"github.com/abema/go-mp4/internal/mp4test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeHEVC(t *testing.T) {
	testCases := []struct {
		name        string
		options     mp4test.HEVCOptions
		codecString string
		encrypted   bool
	}{
		{
			name:        "hvc1",
			options:     mp4test.HEVCOptions{SampleEntry: "hvc1", WithHvcC: true},
			codecString: "hvc1.1.6.L120.90",
		},
		{
			name:        "hev1",
			options:     mp4test.HEVCOptions{SampleEntry: "hev1", WithHvcC: true},
			codecString: "hev1.1.6.L120.90",
		},
		{
			name:        "encv with frma=hvc1",
			options:     mp4test.HEVCOptions{SampleEntry: "encv", OriginalFormat: "hvc1", WithHvcC: true},
			codecString: "hvc1.1.6.L120.90",
			encrypted:   true,
		},
		{
			name:        "encv with frma=hev1",
			options:     mp4test.HEVCOptions{SampleEntry: "encv", OriginalFormat: "hev1", WithHvcC: true},
			codecString: "hev1.1.6.L120.90",
			encrypted:   true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bin := mp4test.BuildHEVC(tc.options)

			info, err := mp4.Probe(bytes.NewReader(bin))
			require.NoError(t, err)
			require.Len(t, info.Tracks, 1)

			tr := info.Tracks[0]
			assert.Equal(t, mp4.CodecHEVC, tr.Codec)
			assert.Equal(t, tc.encrypted, tr.Encrypted)
			assert.Equal(t, tc.codecString, tr.CodecString())

			require.NotNil(t, tr.HEVC)
			assert.Equal(t, uint8(1), tr.HEVC.GeneralProfileIdc)
			assert.Equal(t, uint8(0), tr.HEVC.GeneralProfileSpace)
			assert.False(t, tr.HEVC.GeneralTierFlag)
			assert.Equal(t, uint8(120), tr.HEVC.GeneralLevelIdc)
			assert.Equal(t, uint32(0x60000000), tr.HEVC.GeneralProfileCompatibilityFlags)
			assert.Equal(t, uint64(0x900000000000), tr.HEVC.GeneralConstraintIndicatorFlags)
			assert.Equal(t, uint16(4), tr.HEVC.LengthSize)
			assert.Equal(t, uint16(320), tr.HEVC.Width)
			assert.Equal(t, uint16(180), tr.HEVC.Height)

			idxs, err := mp4.FindRandomAccessFrames(bytes.NewReader(bin), tr)
			require.NoError(t, err)
			// Sample 0 starts with a CRA picture, sample 2 with an IDR_W_RADL,
			// sample 3 with an IDR_N_LP and sample 4 with a BLA_W_LP picture.
			// Sample 5 starts with an access unit delimiter and must not count.
			assert.Equal(t, []int{0, 2, 3, 4}, idxs)

			// FindIDRFrames keeps working for HEVC tracks and uses the same
			// random access semantics.
			idxs, err = mp4.FindIDRFrames(bytes.NewReader(bin), tr)
			require.NoError(t, err)
			assert.Equal(t, []int{0, 2, 3, 4}, idxs)
		})
	}
}

func TestProbeHEVCMissingDecoderConfig(t *testing.T) {
	bin := mp4test.BuildHEVC(mp4test.HEVCOptions{SampleEntry: "hvc1"})

	info, err := mp4.Probe(bytes.NewReader(bin))
	require.NoError(t, err)
	require.Len(t, info.Tracks, 1)

	tr := info.Tracks[0]
	assert.Equal(t, mp4.CodecHEVC, tr.Codec)
	assert.Nil(t, tr.HEVC)
	// Without an hvcC box there is no profile information, so only the sample
	// entry may be reported.
	assert.Equal(t, "hvc1", tr.CodecString())

	idxs, err := mp4.FindRandomAccessFrames(bytes.NewReader(bin), tr)
	require.NoError(t, err)
	assert.Empty(t, idxs)
}

func TestProbeEncryptedAVCIsNotHEVC(t *testing.T) {
	// The original sample_init.encv.mp4 fixture carries frma=avc1.
	f, err := os.Open("./testdata/sample_init.encv.mp4")
	require.NoError(t, err)
	defer f.Close()

	info, err := mp4.Probe(f)
	require.NoError(t, err)
	require.Len(t, info.Tracks, 2)

	assert.Equal(t, mp4.CodecAVC1, info.Tracks[0].Codec)
	assert.True(t, info.Tracks[0].Encrypted)
	assert.Nil(t, info.Tracks[0].HEVC)
	assert.Equal(t, "avc1", info.Tracks[0].CodecString())
}

func TestTrackCodecStringAVCCompatibility(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	info, err := mp4.Probe(f)
	require.NoError(t, err)
	require.Len(t, info.Tracks, 2)

	assert.Equal(t, "avc1.64000C", info.Tracks[0].CodecString())
	assert.Equal(t, "mp4a.40.2", info.Tracks[1].CodecString())
}
