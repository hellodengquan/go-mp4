package mp4

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/orcaman/writerseeker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSidxToMemory runs WriteSidx and returns the produced bytes.
func writeSidxToMemory(t *testing.T, r io.ReadSeeker, opts ...SidxOption) []byte {
	t.Helper()
	w := &writerseeker.WriterSeeker{}
	require.NoError(t, WriteSidx(r, w, opts...))
	out, err := io.ReadAll(w.Reader())
	require.NoError(t, err)
	return out
}

func openTestFile(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("./testdata/" + name)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f
}

// loadSidx extracts the single sidx box from the given bytes.
func loadSidx(t *testing.T, data []byte) *Sidx {
	t.Helper()
	r := bytes.NewReader(data)
	bips, err := ExtractBoxWithPayload(r, nil, BoxPath{BoxTypeSidx()})
	require.NoError(t, err)
	require.Len(t, bips, 1)
	return bips[0].Payload.(*Sidx)
}

func TestWriteSidxDefaultsToFirstVideoTrack(t *testing.T) {
	f := openTestFile(t, "sample_fragmented.mp4")
	out := writeSidxToMemory(t, f)

	sidx := loadSidx(t, out)
	assert.Equal(t, uint8(0), sidx.GetVersion())
	assert.Equal(t, uint32(1), sidx.ReferenceID)
	assert.Equal(t, uint32(90000), sidx.Timescale)
	assert.Equal(t, uint64(18000), sidx.GetEarliestPresentationTime())
	assert.Equal(t, uint64(0), sidx.GetFirstOffset())
	require.Equal(t, 4, len(sidx.References))

	// The sub-segment timing reflects the real video (track 1) samples.
	expectedDurations := []uint32{27000, 18000, 27000, 18000}
	expectedSizes := []uint32{1515, 825, 999, 1104}
	for i, ref := range sidx.References {
		assert.False(t, ref.ReferenceType, "reference[%d] must be a media reference", i)
		assert.Equal(t, expectedDurations[i], ref.SubsegmentDuration, "duration[%d]", i)
		assert.Equal(t, expectedSizes[i], ref.ReferencedSize, "size[%d]", i)
		assert.True(t, ref.StartsWithSAP, "sap[%d]", i)
		assert.Equal(t, uint32(1), ref.SAPType, "sapType[%d]", i)
		assert.Equal(t, uint32(0), ref.SAPDeltaTime, "sapDelta[%d]", i)
	}
}

func TestWriteSidxMuxedTracks(t *testing.T) {
	f := openTestFile(t, "sample_fragmented_muxed.mp4")
	out := writeSidxToMemory(t, f)

	sidx := loadSidx(t, out)
	// Default reference track is the video track.
	assert.Equal(t, uint32(1), sidx.ReferenceID)
	assert.Equal(t, uint32(10240), sidx.Timescale)
	assert.Equal(t, uint64(0), sidx.GetEarliestPresentationTime())
	require.Equal(t, 2, len(sidx.References))

	// Each moof carries both tracks, hence one reference per moof whose byte
	// range contains the complete moof and mdat.
	assert.Equal(t, uint32(4383), sidx.References[0].SubsegmentDuration)
	assert.Equal(t, uint32(2844), sidx.References[0].ReferencedSize)
	assert.Equal(t, uint32(3072), sidx.References[1].SubsegmentDuration)
	assert.Equal(t, uint32(1581), sidx.References[1].ReferencedSize)
}

func TestWriteSidxExplicitReferenceTrack(t *testing.T) {
	f := openTestFile(t, "sample_fragmented.mp4")
	out := writeSidxToMemory(t, f, WithSidxReferenceTrack(2))

	sidx := loadSidx(t, out)
	// The audio track is used as the reference track, so its timescale and
	// sample timing drive the index.
	assert.Equal(t, uint32(2), sidx.ReferenceID)
	assert.Equal(t, uint32(44100), sidx.Timescale)
	assert.Equal(t, uint64(0), sidx.GetEarliestPresentationTime())
	require.Equal(t, 4, len(sidx.References))
	assert.Equal(t, []uint32{13407, 9216, 13312, 17408}, []uint32{
		sidx.References[0].SubsegmentDuration,
		sidx.References[1].SubsegmentDuration,
		sidx.References[2].SubsegmentDuration,
		sidx.References[3].SubsegmentDuration,
	})
}

func TestWriteSidxUnknownReferenceTrack(t *testing.T) {
	f := openTestFile(t, "sample_fragmented.mp4")
	w := &writerseeker.WriterSeeker{}
	err := WriteSidx(f, w, WithSidxReferenceTrack(99))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reference track (trackID=99) not found")
}

func TestWriteSidxRejectsNonFragmentedFile(t *testing.T) {
	f := openTestFile(t, "sample.mp4")
	w := &writerseeker.WriterSeeker{}
	err := WriteSidx(f, w)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no media fragment (moof box) found")
}

func TestWriteSidxInsertionPointAndContiguousRanges(t *testing.T) {
	f := openTestFile(t, "sample_fragmented.mp4")
	out := writeSidxToMemory(t, f)
	r := bytes.NewReader(out)

	// The sidx box must be located after the moov box and before the first
	// media fragment at the root level.
	moovs, err := ExtractBox(r, nil, BoxPath{BoxTypeMoov()})
	require.NoError(t, err)
	require.Len(t, moovs, 1)
	moofs, err := ExtractBox(r, nil, BoxPath{BoxTypeMoof()})
	require.NoError(t, err)
	sidxInfos, err := ExtractBox(r, nil, BoxPath{BoxTypeSidx()})
	require.NoError(t, err)
	require.Len(t, sidxInfos, 1)
	sidxInfo := sidxInfos[0]

	assert.GreaterOrEqual(t, sidxInfo.Offset, moovs[0].Offset+moovs[0].Size)
	assert.Equal(t, moofs[0].Offset, sidxInfo.Offset+sidxInfo.Size)

	// The sidx references must describe byte ranges that are contiguous,
	// start at the first moof and cover every moof/mdat up to the mfra box.
	sidx := loadSidx(t, out)
	running := sidxInfo.Offset + sidxInfo.Size + sidx.GetFirstOffset()
	assert.Equal(t, moofs[0].Offset, running)
	for _, ref := range sidx.References {
		running += uint64(ref.ReferencedSize)
	}
	mfras, err := ExtractBox(r, nil, BoxPath{BoxTypeMfra()})
	require.NoError(t, err)
	require.Len(t, mfras, 1)
	assert.Equal(t, mfras[0].Offset, running)
}

func TestWriteSidxUpdatesMfra(t *testing.T) {
	orig := openTestFile(t, "sample_fragmented.mp4")
	origTfra, err := ExtractBoxWithPayload(orig, nil, BoxPath{BoxTypeMfra(), BoxTypeTfra()})
	require.NoError(t, err)
	origMfro, err := ExtractBoxWithPayload(orig, nil, BoxPath{BoxTypeMfra(), BoxTypeMfro()})
	require.NoError(t, err)

	out := writeSidxToMemory(t, orig)
	r := bytes.NewReader(out)
	sidxInfo, err := ExtractBox(r, nil, BoxPath{BoxTypeSidx()})
	require.NoError(t, err)
	shift := sidxInfo[0].Size

	newTfra, err := ExtractBoxWithPayload(r, nil, BoxPath{BoxTypeMfra(), BoxTypeTfra()})
	require.NoError(t, err)
	newMfro, err := ExtractBoxWithPayload(r, nil, BoxPath{BoxTypeMfra(), BoxTypeMfro()})
	require.NoError(t, err)

	require.Len(t, newTfra, len(origTfra))
	for i := range origTfra {
		oldBox := origTfra[i].Payload.(*Tfra)
		newBox := newTfra[i].Payload.(*Tfra)
		require.Len(t, newBox.Entries, len(oldBox.Entries))
		for j := range oldBox.Entries {
			assert.Equal(t, oldBox.GetMoofOffset(j)+shift, newBox.GetMoofOffset(j),
				"tfra[%d] entry[%d] moof offset", i, j)
			assert.Equal(t, oldBox.GetTime(j), newBox.GetTime(j), "tfra time must not change")
		}
	}
	assert.Equal(t, origMfro[0].Payload.(*Mfro).Size+uint32(shift),
		newMfro[0].Payload.(*Mfro).Size)
}

func TestWriteSidxPreservesMediaPayload(t *testing.T) {
	orig := openTestFile(t, "sample_fragmented_muxed.mp4")
	origMdats, err := ExtractBox(orig, nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)

	out := writeSidxToMemory(t, orig)
	r := bytes.NewReader(out)
	newMdats, err := ExtractBox(r, nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	require.Len(t, newMdats, len(origMdats))

	collect := func(file io.ReadSeeker, boxes []*BoxInfo) []byte {
		var buf bytes.Buffer
		for _, bi := range boxes {
			_, err := file.Seek(int64(bi.Offset+bi.HeaderSize), io.SeekStart)
			require.NoError(t, err)
			_, err = io.CopyN(&buf, file, int64(bi.Size-bi.HeaderSize))
			require.NoError(t, err)
		}
		return buf.Bytes()
	}
	assert.Equal(t, collect(orig, origMdats), collect(r, newMdats))

	// Probe must still parse the output and report the unchanged timing.
	info, err := Probe(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Len(t, info.Segments, 4)
	assert.Equal(t, uint64(0), info.Segments[0].BaseMediaDecodeTime)
	assert.Equal(t, uint32(4383), info.Segments[0].Duration)
}

func TestWriteSidxIsConsistentWhenReapplied(t *testing.T) {
	first := openTestFile(t, "sample_fragmented.mp4")
	once := writeSidxToMemory(t, first)

	// Applying WriteSidx again must preserve the existing sidx and keep both
	// indexes mutually consistent.
	twice := writeSidxToMemory(t, bytes.NewReader(once))

	onceSidx := loadSidx(t, once)
	r := bytes.NewReader(twice)
	allSidx, err := ExtractBoxWithPayload(r, nil, BoxPath{BoxTypeSidx()})
	require.NoError(t, err)
	require.Len(t, allSidx, 2)
	for _, b := range allSidx {
		s := b.Payload.(*Sidx)
		assert.Equal(t, onceSidx.ReferenceID, s.ReferenceID)
		assert.Equal(t, onceSidx.Timescale, s.Timescale)
		assert.Equal(t, onceSidx.GetEarliestPresentationTime(), s.GetEarliestPresentationTime())
		assert.Len(t, s.References, len(onceSidx.References))
		for i, ref := range s.References {
			assert.Equal(t, onceSidx.References[i].ReferencedSize, ref.ReferencedSize)
			assert.Equal(t, onceSidx.References[i].SubsegmentDuration, ref.SubsegmentDuration)
		}
	}
}

func TestSidxTrafSAP(t *testing.T) {
	tfhd := &Tfhd{}

	t.Run("starts with sync sample via first sample flags", func(t *testing.T) {
		trun := &Trun{}
		trun.SetFlags(0x000004)
		trun.FirstSampleFlags = 0x02000000 // sample_depends_on = 2
		trun.SampleCount = 1
		trun.Entries = []TrunEntry{{SampleDuration: 100}}

		sap, sapType, sapDelta := sidxTrafSAP(tfhd, trun)
		assert.True(t, sap)
		assert.Equal(t, uint32(1), sapType)
		assert.Equal(t, uint32(0), sapDelta)
	})

	t.Run("sync sample found after non sync samples", func(t *testing.T) {
		trun := &Trun{}
		trun.SetFlags(0x000500) // per-sample duration and flags
		trun.SampleCount = 3
		trun.Entries = []TrunEntry{
			{SampleDuration: 100, SampleFlags: 0x01000000}, // depends on others
			{SampleDuration: 100, SampleFlags: 0x01000000},
			{SampleDuration: 100, SampleFlags: 0x02000000}, // sync sample
		}

		sap, sapType, sapDelta := sidxTrafSAP(tfhd, trun)
		assert.False(t, sap)
		assert.Equal(t, uint32(1), sapType)
		assert.Equal(t, uint32(200), sapDelta)
	})

	t.Run("no sync sample in fragment", func(t *testing.T) {
		trun := &Trun{}
		trun.SetFlags(0x000500)
		trun.SampleCount = 2
		trun.Entries = []TrunEntry{
			{SampleDuration: 100, SampleFlags: 0x01000000},
			{SampleDuration: 100, SampleFlags: 0x01000000},
		}

		sap, sapType, sapDelta := sidxTrafSAP(tfhd, trun)
		assert.False(t, sap)
		assert.Equal(t, uint32(0), sapType)
		assert.Equal(t, uint32(0), sapDelta)
	})
}

func TestSidxTrafDuration(t *testing.T) {
	tfhd := &Tfhd{DefaultSampleDuration: 9000}

	t.Run("uses trun sample durations", func(t *testing.T) {
		trun := &Trun{}
		trun.SetFlags(0x000100)
		trun.SampleCount = 3
		trun.Entries = []TrunEntry{
			{SampleDuration: 100}, {SampleDuration: 200}, {SampleDuration: 300},
		}
		assert.Equal(t, uint32(600), sidxTrafDuration(tfhd, trun))
	})

	t.Run("falls back to tfhd default duration", func(t *testing.T) {
		trun := &Trun{SampleCount: 3}
		assert.Equal(t, uint32(27000), sidxTrafDuration(tfhd, trun))
	})
}

func TestSidxPresentationTime(t *testing.T) {
	assert.Equal(t, uint64(18000), sidxPresentationTime(0, 18000))
	assert.Equal(t, uint64(500), sidxPresentationTime(1000, -500))
	// Negative offset must never underflow.
	assert.Equal(t, uint64(0), sidxPresentationTime(100, -200))
	assert.Equal(t, uint64(0), sidxPresentationTime(100, -100))
}

func TestSelectSidxReferenceTrack(t *testing.T) {
	video := &sidxTrack{trackID: 1, handler: [4]byte{'v', 'i', 'd', 'e'}}
	audio := &sidxTrack{trackID: 2, handler: [4]byte{'s', 'o', 'u', 'n'}}
	tracks := map[uint32]*sidxTrack{1: video, 2: audio}

	t.Run("explicit track wins", func(t *testing.T) {
		got, err := selectSidxReferenceTrack(tracks, &sidxConfig{referenceTrackID: 2, trackSpecified: true})
		require.NoError(t, err)
		assert.Equal(t, audio, got)
	})

	t.Run("defaults to first video track", func(t *testing.T) {
		got, err := selectSidxReferenceTrack(tracks, &sidxConfig{})
		require.NoError(t, err)
		assert.Equal(t, video, got)
	})

	t.Run("falls back to first track without video", func(t *testing.T) {
		onlyAudio := map[uint32]*sidxTrack{2: audio}
		got, err := selectSidxReferenceTrack(onlyAudio, &sidxConfig{})
		require.NoError(t, err)
		assert.Equal(t, audio, got)
	})

	t.Run("unknown explicit track errors", func(t *testing.T) {
		_, err := selectSidxReferenceTrack(tracks, &sidxConfig{referenceTrackID: 99, trackSpecified: true})
		require.Error(t, err)
	})
}

func TestRewriteStcoAndCo64(t *testing.T) {
	const shift = uint64(80)

	t.Run("stco shifts every chunk offset", func(t *testing.T) {
		src := &Stco{
			EntryCount:  3,
			ChunkOffset: []uint32{1000, 2000, 3000},
		}
		bi := marshalBoxForSidxTest(t, src)

		r := bytes.NewReader(bi)
		readBI := &BoxInfo{Offset: 0, Size: uint64(len(bi)), HeaderSize: SmallHeaderSize, Type: BoxTypeStco()}
		w := &writerseeker.WriterSeeker{}
		require.NoError(t, rewriteStco(r, NewWriter(w), readBI, shift))

		out, err := io.ReadAll(w.Reader())
		require.NoError(t, err)
		dst := &Stco{}
		_, err = Unmarshal(bytes.NewReader(out[SmallHeaderSize:]), uint64(len(out)-SmallHeaderSize), dst, Context{})
		require.NoError(t, err)
		assert.Equal(t, []uint32{1080, 2080, 3080}, dst.ChunkOffset)
	})

	t.Run("co64 shifts every chunk offset", func(t *testing.T) {
		src := &Co64{
			EntryCount:  2,
			ChunkOffset: []uint64{5000, 6000},
		}
		data := marshalBoxForSidxTest(t, src)

		r := bytes.NewReader(data)
		readBI := &BoxInfo{Offset: 0, Size: uint64(len(data)), HeaderSize: SmallHeaderSize, Type: BoxTypeCo64()}
		w := &writerseeker.WriterSeeker{}
		require.NoError(t, rewriteCo64(r, NewWriter(w), readBI, shift))

		out, err := io.ReadAll(w.Reader())
		require.NoError(t, err)
		dst := &Co64{}
		_, err = Unmarshal(bytes.NewReader(out[SmallHeaderSize:]), uint64(len(out)-SmallHeaderSize), dst, Context{})
		require.NoError(t, err)
		assert.Equal(t, []uint64{5080, 6080}, dst.ChunkOffset)
	})
}

func TestRewriteTfhdBaseDataOffset(t *testing.T) {
	const shift = uint64(80)

	t.Run("shifts base_data_offset when present", func(t *testing.T) {
		src := &Tfhd{
			TrackID:        1,
			BaseDataOffset: 1234,
		}
		src.AddFlag(TfhdBaseDataOffsetPresent)
		data := marshalBoxForSidxTest(t, src)

		r := bytes.NewReader(data)
		readBI := &BoxInfo{Offset: 0, Size: uint64(len(data)), HeaderSize: SmallHeaderSize, Type: BoxTypeTfhd()}
		w := &writerseeker.WriterSeeker{}
		require.NoError(t, rewriteTfhd(r, NewWriter(w), readBI, shift))

		out, err := io.ReadAll(w.Reader())
		require.NoError(t, err)
		dst := &Tfhd{}
		_, err = Unmarshal(bytes.NewReader(out[SmallHeaderSize:]), uint64(len(out)-SmallHeaderSize), dst, Context{})
		require.NoError(t, err)
		assert.True(t, dst.CheckFlag(TfhdBaseDataOffsetPresent))
		assert.Equal(t, uint64(1314), dst.BaseDataOffset)
		assert.Equal(t, uint32(1), dst.TrackID)
	})

	t.Run("leaves tfhd without base_data_offset untouched", func(t *testing.T) {
		src := &Tfhd{TrackID: 1, DefaultSampleDuration: 9000}
		src.AddFlag(TfhdDefaultSampleDurationPresent)
		data := marshalBoxForSidxTest(t, src)

		r := bytes.NewReader(data)
		readBI := &BoxInfo{Offset: 0, Size: uint64(len(data)), HeaderSize: SmallHeaderSize, Type: BoxTypeTfhd()}
		w := &writerseeker.WriterSeeker{}
		require.NoError(t, rewriteTfhd(r, NewWriter(w), readBI, shift))

		out, err := io.ReadAll(w.Reader())
		require.NoError(t, err)
		dst := &Tfhd{}
		_, err = Unmarshal(bytes.NewReader(out[SmallHeaderSize:]), uint64(len(out)-SmallHeaderSize), dst, Context{})
		require.NoError(t, err)
		assert.False(t, dst.CheckFlag(TfhdBaseDataOffsetPresent))
		assert.Equal(t, uint32(9000), dst.DefaultSampleDuration)
	})
}

// marshalBoxForSidxTest serializes one box including its header into memory.
func marshalBoxForSidxTest(t *testing.T, box IImmutableBox) []byte {
	t.Helper()
	w := &writerseeker.WriterSeeker{}
	writer := NewWriter(w)
	_, err := writer.StartBox(&BoxInfo{Type: box.GetType()})
	require.NoError(t, err)
	_, err = Marshal(writer, box, Context{})
	require.NoError(t, err)
	_, err = writer.EndBox()
	require.NoError(t, err)
	data, err := io.ReadAll(w.Reader())
	require.NoError(t, err)
	return data
}

func TestSidxMoofNeedsTfhdRewrite(t *testing.T) {
	buildMoof := func(setBaseDataOffset bool) []byte {
		w := &writerseeker.WriterSeeker{}
		writer := NewWriter(w)
		require.NoError(t, writeNestedSidxBox(writer, BoxTypeMoof(), func() error {
			return writeNestedSidxBox(writer, BoxTypeTraf(), func() error {
				tfhd := &Tfhd{TrackID: 1}
				if setBaseDataOffset {
					tfhd.AddFlag(TfhdBaseDataOffsetPresent)
					tfhd.BaseDataOffset = 5000
				}
				if _, err := writer.StartBox(&BoxInfo{Type: BoxTypeTfhd()}); err != nil {
					return err
				}
				if _, err := Marshal(writer, tfhd, Context{}); err != nil {
					return err
				}
				_, err := writer.EndBox()
				return err
			})
		}))
		data, err := io.ReadAll(w.Reader())
		require.NoError(t, err)
		return data
	}

	t.Run("detects base_data_offset", func(t *testing.T) {
		data := buildMoof(true)
		r := bytes.NewReader(data)
		bis, err := ExtractBox(r, nil, BoxPath{BoxTypeMoof()})
		require.NoError(t, err)
		needs, err := sidxMoofNeedsTfhdRewrite(r, bis[0])
		require.NoError(t, err)
		assert.True(t, needs)
	})

	t.Run("no base_data_offset present", func(t *testing.T) {
		data := buildMoof(false)
		r := bytes.NewReader(data)
		bis, err := ExtractBox(r, nil, BoxPath{BoxTypeMoof()})
		require.NoError(t, err)
		needs, err := sidxMoofNeedsTfhdRewrite(r, bis[0])
		require.NoError(t, err)
		assert.False(t, needs)
	})
}

// writeNestedSidxBox opens a container box, runs fn to write its children and
// closes the box again.
func writeNestedSidxBox(w *Writer, typ BoxType, fn func() error) error {
	if _, err := w.StartBox(&BoxInfo{Type: typ}); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}
