// Package mp4test builds small synthetic MP4 files for tests.
//
// The builder is used to exercise the probe without depending on external
// media files or an HEVC encoder.
package mp4test

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"
	"github.com/orcaman/writerseeker"
)

// HEVCOptions describes the HEVC track to build.
type HEVCOptions struct {
	// SampleEntry is the visual sample entry type: "hvc1", "hev1" or "encv".
	SampleEntry string

	// OriginalFormat is the format recorded in the frma box of an encrypted
	// sample entry.  It is ignored for plain sample entries.
	OriginalFormat string

	// WithHvcC controls whether the hvcC decoder configuration box is written.
	WithHvcC bool

	// Fragmented builds a fragmented MP4 (moof/mdat media segments) instead of
	// a plain one.
	Fragmented bool
}

// BuildHEVC returns the bytes of a single track HEVC MP4 file.
//
// The media data consists of six samples.  Samples 0, 2, 3 and 4 start with an
// independently decodable random access picture (CRA, IDR_W_RADL, IDR_N_LP and
// BLA_W_LP respectively), while samples 1 and 5 carry only non random access
// pictures.  The decoder configuration describes Main profile, level 120 with
// the 0x90 constraint byte.
func BuildHEVC(opts HEVCOptions) []byte {
	sampleEntry := opts.SampleEntry
	if sampleEntry == "" {
		sampleEntry = "hvc1"
	}

	if opts.Fragmented {
		return buildFragmentedHEVC(sampleEntry, opts)
	}

	samples := buildSamples()
	sampleSizes := make([]uint32, len(samples))

	ws := &writerseeker.WriterSeeker{}
	w := mp4.NewWriter(ws)

	writeFtyp(w)

	// mdat is written before moov so that the chunk offset is known when the
	// sample tables are written.  The chunk offset points at the first sample,
	// i.e. at the payload of the mdat box.
	_, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()})
	if err != nil {
		panic(err)
	}
	mdatPayload, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		panic(err)
	}
	for i, sample := range samples {
		sampleSizes[i] = uint32(len(sample))
		if _, err := w.Write(sample); err != nil {
			panic(err)
		}
	}
	if _, err := w.EndBox(); err != nil {
		panic(err)
	}

	writeMoov(w, sampleEntry, opts, uint64(mdatPayload), sampleSizes)

	return mustReadAll(ws)
}

func mustReadAll(ws *writerseeker.WriterSeeker) []byte {
	b, err := io.ReadAll(ws.Reader())
	if err != nil {
		panic(err)
	}
	return b
}

func writeFtyp(w *mp4.Writer) {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		panic(err)
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', '5'},
		MinorVersion: 512,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}},
			{CompatibleBrand: [4]byte{'h', 'v', 'c', '1'}},
			{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}},
		},
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		panic(err)
	}
	if _, err := w.EndBox(); err != nil {
		panic(err)
	}
}

func writeMoov(w *mp4.Writer, sampleEntry string, opts HEVCOptions, dataOffset uint64, sampleSizes []uint32) {
	startBox(w, mp4.BoxTypeMoov())
	defer endBox(w)

	startBox(w, mp4.BoxTypeMvhd())
	marshal(w, &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  1000,
		Rate:        0x00010000,
		Volume:      0x0100,
		NextTrackID: 2,
	})
	endBox(w)

	startBox(w, mp4.BoxTypeTrak())
	defer endBox(w)

	startBox(w, mp4.BoxTypeTkhd())
	marshal(w, &mp4.Tkhd{
		TrackID:    1,
		DurationV0: 1000,
	})
	endBox(w)

	startBox(w, mp4.BoxTypeMdia())
	defer endBox(w)

	startBox(w, mp4.BoxTypeMdhd())
	marshal(w, &mp4.Mdhd{
		Timescale:  90000,
		DurationV0: uint32(len(sampleSizes) * 3000),
		Language:   [3]byte{21, 14, 4}, // "und"
	})
	endBox(w)

	startBox(w, mp4.BoxTypeMinf())
	defer endBox(w)

	startBox(w, mp4.BoxTypeStbl())
	defer endBox(w)

	writeStsd(w, sampleEntry, opts)
	writeStts(w, len(sampleSizes))
	writeStsc(w, len(sampleSizes))
	writeStsz(w, sampleSizes)
	writeStco(w, dataOffset)
}

func writeStsd(w *mp4.Writer, sampleEntry string, opts HEVCOptions) {
	startBox(w, mp4.BoxTypeStsd())
	defer endBox(w)

	marshal(w, &mp4.Stsd{EntryCount: 1})

	entryType := mp4.StrToBoxType(sampleEntry)
	startBox(w, entryType)
	marshal(w, &mp4.VisualSampleEntry{
		SampleEntry: mp4.SampleEntry{
			AnyTypeBox:         mp4.AnyTypeBox{Type: entryType},
			DataReferenceIndex: 1,
		},
		Width:      320,
		Height:     180,
		FrameCount: 1,
	})

	if opts.WithHvcC {
		startBox(w, mp4.BoxTypeHvcC())
		marshal(w, buildHvcC())
		endBox(w)
	}
	if sampleEntry == "encv" {
		startBox(w, mp4.BoxTypeSinf())
		startBox(w, mp4.BoxTypeFrma())
		marshal(w, &mp4.Frma{DataFormat: mp4.StrToBoxType(opts.OriginalFormat)})
		endBox(w)
		endBox(w)
	}
	endBox(w)
}

func buildHvcC() *mp4.HvcC {
	profileCompatibility := [32]bool{}
	profileCompatibility[1] = true
	profileCompatibility[2] = true

	return &mp4.HvcC{
		ConfigurationVersion:        1,
		GeneralProfileSpace:         0,
		GeneralTierFlag:             false,
		GeneralProfileIdc:           1, // Main
		GeneralProfileCompatibility: profileCompatibility,
		GeneralConstraintIndicator:  [6]uint8{0x90, 0, 0, 0, 0, 0},
		GeneralLevelIdc:             120,
		Reserved1:                   0xf,
		Reserved2:                   0x3f,
		Reserved3:                   0x3f,
		ChromaFormatIdc:             1,
		Reserved4:                   0x1f,
		Reserved5:                   0x1f,
		NumTemporalLayers:           1,
		TemporalIdNested:            1,
		LengthSizeMinusOne:          3,
		NumOfNaluArrays:             3,
		NaluArrays: []mp4.HEVCNaluArray{
			naluArray(32, []byte{0x40, 0x01, 0x0c, 0x01}),
			naluArray(33, []byte{0x42, 0x01, 0x01, 0x60}),
			naluArray(34, []byte{0x44, 0x01, 0xc1}),
		},
	}
}

func naluArray(naluType uint8, payload []byte) mp4.HEVCNaluArray {
	return mp4.HEVCNaluArray{
		Completeness: true,
		NaluType:     naluType,
		NumNalus:     1,
		Nalus: []mp4.HEVCNalu{{
			Length:  uint16(len(payload)),
			NALUnit: payload,
		}},
	}
}

func writeStts(w *mp4.Writer, count int) {
	startBox(w, mp4.BoxTypeStts())
	defer endBox(w)

	marshal(w, &mp4.Stts{
		EntryCount: 1,
		Entries: []mp4.SttsEntry{
			{SampleCount: uint32(count), SampleDelta: 3000},
		},
	})
}

func writeStsc(w *mp4.Writer, count int) {
	startBox(w, mp4.BoxTypeStsc())
	defer endBox(w)

	marshal(w, &mp4.Stsc{
		EntryCount: 1,
		Entries: []mp4.StscEntry{
			{FirstChunk: 1, SamplesPerChunk: uint32(count), SampleDescriptionIndex: 1},
		},
	})
}

func writeStsz(w *mp4.Writer, sampleSizes []uint32) {
	startBox(w, mp4.BoxTypeStsz())
	defer endBox(w)

	marshal(w, &mp4.Stsz{
		SampleSize:  0,
		SampleCount: uint32(len(sampleSizes)),
		EntrySize:   sampleSizes,
	})
}

func writeStco(w *mp4.Writer, dataOffset uint64) {
	startBox(w, mp4.BoxTypeStco())
	defer endBox(w)

	marshal(w, &mp4.Stco{
		EntryCount:  1,
		ChunkOffset: []uint32{uint32(dataOffset)},
	})
}

// buildSamples returns the length prefixed NAL units of each sample.
func buildSamples() [][]byte {
	return [][]byte{
		// CRA access unit with parameter sets and a prefix SEI.
		joinNalus(
			nalu(32, 4), // VPS
			nalu(33, 6), // SPS
			nalu(34, 3), // PPS
			nalu(39, 4), // prefix SEI
			nalu(21, 8), // CRA_NUT
		),
		// Non random access picture.
		joinNalus(nalu(1, 8)), // TRAIL_R
		// IDR_W_RADL.
		joinNalus(nalu(19, 8)),
		// IDR_N_LP.
		joinNalus(nalu(20, 8)),
		// BLA_W_LP.
		joinNalus(nalu(16, 8)),
		// Access unit delimiter followed by a non random access picture.
		joinNalus(
			nalu(35, 2), // AUD
			nalu(0, 8),  // TRAIL_N
		),
	}
}

// nalu returns a length prefixed HEVC NAL unit with a two byte header.
func nalu(naluType uint8, payloadLen int) []byte {
	buf := make([]byte, 4+2+payloadLen)
	binary.BigEndian.PutUint32(buf, uint32(2+payloadLen))
	buf[4] = naluType << 1
	buf[5] = 0x01 // nuh_layer_id = 0, nuh_temporal_id_plus1 = 1
	return buf
}

func joinNalus(nalus ...[]byte) []byte {
	var out []byte
	for _, nalu := range nalus {
		out = append(out, nalu...)
	}
	return out
}

func startBox(w *mp4.Writer, boxType mp4.BoxType) {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: boxType}); err != nil {
		panic(err)
	}
}

func endBox(w *mp4.Writer) {
	if _, err := w.EndBox(); err != nil {
		panic(err)
	}
}

func marshal(w *mp4.Writer, box mp4.IImmutableBox) {
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		panic(err)
	}
}

// NewReader returns a read seeker over the given file bytes.
func NewReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}

// buildFragmentedHEVC returns a fragmented HEVC MP4 file with a single moof
// carrying all six samples.
func buildFragmentedHEVC(sampleEntry string, opts HEVCOptions) []byte {
	samples := buildSamples()

	ws := &writerseeker.WriterSeeker{}
	w := mp4.NewWriter(ws)

	writeFtyp(w)
	writeFragmentedMoov(w, sampleEntry, opts)
	writeMoof(w, samples)

	return mustReadAll(ws)
}

func writeFragmentedMoov(w *mp4.Writer, sampleEntry string, opts HEVCOptions) {
	startBox(w, mp4.BoxTypeMoov())

	startBox(w, mp4.BoxTypeMvhd())
	marshal(w, &mp4.Mvhd{
		Timescale:   1000,
		DurationV0:  0,
		Rate:        0x00010000,
		Volume:      0x0100,
		NextTrackID: 2,
	})
	endBox(w)

	startBox(w, mp4.BoxTypeTrak())
	startBox(w, mp4.BoxTypeTkhd())
	marshal(w, &mp4.Tkhd{TrackID: 1})
	endBox(w)

	startBox(w, mp4.BoxTypeMdia())
	startBox(w, mp4.BoxTypeMdhd())
	marshal(w, &mp4.Mdhd{
		Timescale: 90000,
		Language:  [3]byte{21, 14, 4}, // "und"
	})
	endBox(w)

	startBox(w, mp4.BoxTypeMinf())
	startBox(w, mp4.BoxTypeStbl())

	writeStsd(w, sampleEntry, opts)
	writeEmptyStts(w)
	writeEmptyStsc(w)
	writeEmptyStsz(w)
	writeEmptyStco(w)

	endBox(w) // stbl
	endBox(w) // minf
	endBox(w) // mdia
	endBox(w) // trak

	startBox(w, mp4.BoxTypeMvex())
	startBox(w, mp4.BoxTypeTrex())
	marshal(w, &mp4.Trex{
		TrackID:                       1,
		DefaultSampleDescriptionIndex: 1,
	})
	endBox(w) // trex
	endBox(w) // mvex
	endBox(w) // moov
}

func writeEmptyStts(w *mp4.Writer) {
	startBox(w, mp4.BoxTypeStts())
	marshal(w, &mp4.Stts{})
	endBox(w)
}

func writeEmptyStsc(w *mp4.Writer) {
	startBox(w, mp4.BoxTypeStsc())
	marshal(w, &mp4.Stsc{})
	endBox(w)
}

func writeEmptyStsz(w *mp4.Writer) {
	startBox(w, mp4.BoxTypeStsz())
	marshal(w, &mp4.Stsz{})
	endBox(w)
}

func writeEmptyStco(w *mp4.Writer) {
	startBox(w, mp4.BoxTypeStco())
	marshal(w, &mp4.Stco{})
	endBox(w)
}

func writeMoof(w *mp4.Writer, samples [][]byte) {
	// The trun data offset is relative to the start of the moof box, so the
	// moof is first marshalled into a scratch buffer to learn its size.  The
	// value of the data offset does not affect the size, so a second pass with
	// the real offset produces an identical layout.
	scratch := &writerseeker.WriterSeeker{}
	writeMoofBody(mp4.NewWriter(scratch), samples, 0)
	moofSize := len(mustReadAll(scratch))

	writeMoofBody(w, samples, int32(moofSize+8))
}

func writeMoofBody(w *mp4.Writer, samples [][]byte, dataOffset int32) {
	startBox(w, mp4.BoxTypeMoof())

	startBox(w, mp4.BoxTypeMfhd())
	marshal(w, &mp4.Mfhd{SequenceNumber: 1})
	endBox(w)

	startBox(w, mp4.BoxTypeTraf())

	startBox(w, mp4.BoxTypeTfhd())
	tfhd := &mp4.Tfhd{TrackID: 1}
	tfhd.SetFlags(mp4.TfhdDefaultSampleDurationPresent)
	tfhd.DefaultSampleDuration = 3000
	marshal(w, tfhd)
	endBox(w)

	startBox(w, mp4.BoxTypeTfdt())
	marshal(w, &mp4.Tfdt{BaseMediaDecodeTimeV0: 0})
	endBox(w)

	startBox(w, mp4.BoxTypeTrun())
	trun := &mp4.Trun{}
	trun.SetFlags(0x000001 | 0x000100 | 0x000200) // data offset, duration, size
	trun.SampleCount = uint32(len(samples))
	trun.DataOffset = dataOffset
	entries := make([]mp4.TrunEntry, len(samples))
	for i, sample := range samples {
		entries[i] = mp4.TrunEntry{SampleDuration: 3000, SampleSize: uint32(len(sample))}
	}
	trun.Entries = entries
	marshal(w, trun)
	endBox(w) // trun

	endBox(w) // traf
	endBox(w) // moof

	startBox(w, mp4.BoxTypeMdat())
	for _, sample := range samples {
		if _, err := w.Write(sample); err != nil {
			panic(err)
		}
	}
	endBox(w)
}
