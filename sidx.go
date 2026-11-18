package mp4

import (
	"errors"
	"fmt"
	"io"
)

// SidxOption configures the behavior of WriteSidx.
type SidxOption func(*sidxConfig)

type sidxConfig struct {
	referenceTrackID uint32
	trackSpecified   bool
}

// WithSidxReferenceTrack specifies the track ID used as the reference track
// of the Segment Index Box (sidx). When this option is not given,
// WriteSidx uses the first video track declared in the moov box.
func WithSidxReferenceTrack(trackID uint32) SidxOption {
	return func(c *sidxConfig) {
		c.referenceTrackID = trackID
		c.trackSpecified = true
	}
}

// sidxTrack keeps the static properties of a track declared in the moov box.
type sidxTrack struct {
	trackID   uint32
	timescale uint32
	handler   [4]byte
}

// sidxFragment represents one media fragment: a moof box and all the boxes
// following it (typically an mdat box) up to the next fragment or to the
// mfra box. Every fragment that contains a track fragment of the reference
// track starts a sub-segment; fragments of the other tracks located between
// two reference fragments are merged into the preceding sub-segment so that
// the byte ranges of the sidx references stay contiguous and cover the
// complete media.
type sidxFragment struct {
	moofInfo          *BoxInfo
	endOffset         uint64
	hasReferenceTrack bool
	referenceTrafInfo *sidxTrafInfo
}

// sidxTrafInfo contains the timing and SAP information of the reference
// track inside one moof box. All values are expressed in the reference
// track's media time units.
type sidxTrafInfo struct {
	baseMediaDecodeTime  uint64
	duration             uint32
	firstCompositionTime int64
	startsWithSAP        bool
	sapType              uint32
	sapDeltaTime         uint32
}

// WriteSidx generates a Segment Index Box (sidx) describing the media
// fragments of the fragmented MP4 read from r and writes the whole file,
// including the inserted sidx box, to w.
//
// The sub-segments are divided on the media timeline of the reference track.
// Each moof box containing a track fragment of the reference track produces
// one sidx reference. The byte range of a reference covers the complete
// moof box and the following media data, therefore the interleaved data of
// the other tracks are also included. Sub-segment duration, earliest
// presentation time and stream access point (SAP) information reflect the
// real samples of the reference track; interleaving of other tracks never
// changes the meaning of these time values.
//
// By default, the first video track is used as the reference track.
// The WithSidxReferenceTrack option overrides it.
//
// Absolute offsets kept by the original addressing boxes (tfra, mfro,
// stco and co64) are updated so that they keep pointing at the correct
// media after the sidx box insertion changes the file layout. Boxes already
// present in the input, including existing sidx boxes, are preserved; their
// relative offsets stay valid and their meaning is consistent with the
// newly generated index.
func WriteSidx(r io.ReadSeeker, w io.WriteSeeker, opts ...SidxOption) error {
	cfg := sidxConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	tracks, fragments, err := collectSidxInfo(r)
	if err != nil {
		return err
	}
	if len(fragments) == 0 {
		return errors.New("no media fragment (moof box) found")
	}

	refTrack, err := selectSidxReferenceTrack(tracks, &cfg)
	if err != nil {
		return err
	}

	sidx, err := buildSidx(r, refTrack, fragments)
	if err != nil {
		return err
	}
	if sidx == nil {
		return fmt.Errorf("no track fragment of the reference track (trackID=%d) found", refTrack.trackID)
	}

	return rewriteWithSidx(r, w, fragments[0].moofInfo.Offset, sidx)
}

// collectSidxInfo scans the root boxes of the file and collects the static
// track information from the moov box and the byte ranges of every media
// fragment. A fragment spans from its moof box up to the next moof box,
// the mfra box or the end of the file, so its byte range always contains
// the complete media payload.
func collectSidxInfo(r io.ReadSeeker) (map[uint32]*sidxTrack, []*sidxFragment, error) {
	var rootBoxes []*BoxInfo
	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		if len(h.Path) != 1 {
			return nil, nil
		}
		box := h.BoxInfo
		rootBoxes = append(rootBoxes, &box)
		return nil, nil
	})
	if err != nil {
		return nil, nil, err
	}

	var moovInfo *BoxInfo
	for _, bi := range rootBoxes {
		if bi.Type == BoxTypeMoov() {
			moovInfo = bi
			break
		}
	}
	if moovInfo == nil {
		return nil, nil, errors.New("moov box not found")
	}

	trakInfos, err := ExtractBox(r, moovInfo, BoxPath{BoxTypeTrak()})
	if err != nil {
		return nil, nil, err
	}
	tracks := make(map[uint32]*sidxTrack, len(trakInfos))
	for _, trakInfo := range trakInfos {
		track, err := probeSidxTrack(r, trakInfo)
		if err != nil {
			return nil, nil, err
		}
		tracks[track.trackID] = track
	}

	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, err
	}
	var fragments []*sidxFragment
	for i, bi := range rootBoxes {
		if bi.Type != BoxTypeMoof() {
			continue
		}
		endOffset := uint64(fileSize)
		for j := i + 1; j < len(rootBoxes); j++ {
			// A fragment ends at the next fragment, at an existing sidx box
			// (which is index metadata, not media payload) or at the mfra box.
			if rootBoxes[j].Type == BoxTypeMoof() ||
				rootBoxes[j].Type == BoxTypeSidx() ||
				rootBoxes[j].Type == BoxTypeMfra() {
				endOffset = rootBoxes[j].Offset
				break
			}
		}
		fragments = append(fragments, &sidxFragment{
			moofInfo:  bi,
			endOffset: endOffset,
		})
	}
	return tracks, fragments, nil
}

// probeSidxTrack extracts the track ID, timescale and handler type from a trak box.
func probeSidxTrack(r io.ReadSeeker, trak *BoxInfo) (*sidxTrack, error) {
	bips, err := ExtractBoxesWithPayload(r, trak, []BoxPath{
		{BoxTypeTkhd()},
		{BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMdia(), BoxTypeHdlr()},
	})
	if err != nil {
		return nil, err
	}

	track := &sidxTrack{}
	var tkhdFound, mdhdFound bool
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTkhd():
			track.trackID = bip.Payload.(*Tkhd).TrackID
			tkhdFound = true
		case BoxTypeMdhd():
			track.timescale = bip.Payload.(*Mdhd).Timescale
			mdhdFound = true
		case BoxTypeHdlr():
			track.handler = bip.Payload.(*Hdlr).HandlerType
		}
	}
	if !tkhdFound {
		return nil, errors.New("tkhd box not found")
	}
	if !mdhdFound {
		return nil, errors.New("mdhd box not found")
	}
	return track, nil
}

// selectSidxReferenceTrack resolves the reference track. An explicitly
// specified track ID wins; otherwise the first video track (hdlr handler_type
// "vide") in track order is used, falling back to the first track when the
// file has no video track.
func selectSidxReferenceTrack(tracks map[uint32]*sidxTrack, cfg *sidxConfig) (*sidxTrack, error) {
	if cfg.trackSpecified {
		track, ok := tracks[cfg.referenceTrackID]
		if !ok {
			return nil, fmt.Errorf("reference track (trackID=%d) not found", cfg.referenceTrackID)
		}
		return track, nil
	}

	orderedIDs := sidxOrderedTrackIDs(tracks)
	if len(orderedIDs) == 0 {
		return nil, errors.New("no track found")
	}
	for _, id := range orderedIDs {
		if tracks[id].handler == [4]byte{'v', 'i', 'd', 'e'} {
			return tracks[id], nil
		}
	}
	return tracks[orderedIDs[0]], nil
}

// sidxOrderedTrackIDs returns the track IDs sorted by numeric ID. Track IDs
// are assigned in file order in practice, and numeric ordering keeps the
// selection deterministic.
func sidxOrderedTrackIDs(tracks map[uint32]*sidxTrack) []uint32 {
	ids := make([]uint32, 0, len(tracks))
	for id := range tracks {
		ids = append(ids, id)
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return ids
}

// buildSidx creates the sidx box payload from the media fragments.
//
// Every fragment containing a track fragment of the reference track starts
// a sub-segment. A fragment of another track located between two reference
// fragments is merged into the preceding sub-segment: this keeps the byte
// ranges contiguous and covering the whole media, while the timing fields
// remain derived from the reference track samples only. It returns nil when
// none of the fragments contains the reference track.
func buildSidx(r io.ReadSeeker, refTrack *sidxTrack, fragments []*sidxFragment) (*Sidx, error) {
	var referenceCount int
	useVersion1 := false
	for _, fragment := range fragments {
		info, err := probeSidxReferenceTraf(r, fragment.moofInfo, refTrack.trackID)
		if err != nil {
			return nil, err
		}
		fragment.referenceTrafInfo = info
		fragment.hasReferenceTrack = info != nil
		if info == nil {
			continue
		}
		referenceCount++

		presentationTime := sidxPresentationTime(info.baseMediaDecodeTime, info.firstCompositionTime)
		if presentationTime > uint64(^uint32(0)) ||
			fragment.endOffset > uint64(^uint32(0)) ||
			fragment.moofInfo.Offset > uint64(^uint32(0)) {
			useVersion1 = true
		}
	}
	if referenceCount == 0 {
		return nil, nil
	}
	if referenceCount > 65535 {
		return nil, fmt.Errorf("too many sub-segments for a single sidx box: %d (max 65535)", referenceCount)
	}

	references := make([]SidxReference, 0, referenceCount)
	var earliestPresentationTime uint64
	// subsegmentStart is the index of the fragment opening the current
	// sub-segment. The first reference fragment opens the first
	// sub-segment; each following reference fragment opens a new one.
	subsegmentStart := -1
	for i, fragment := range fragments {
		if !fragment.hasReferenceTrack {
			continue
		}
		if subsegmentStart == -1 {
			subsegmentStart = i
		}
		if len(references) == 0 {
			earliestPresentationTime = sidxPresentationTime(
				fragment.referenceTrafInfo.baseMediaDecodeTime,
				fragment.referenceTrafInfo.firstCompositionTime)
		}

		// The sub-segment ends just before the next reference fragment,
		// or at the end of the last fragment.
		subsegmentEnd := fragment.endOffset
		for j := i + 1; j < len(fragments); j++ {
			if fragments[j].hasReferenceTrack {
				break
			}
			subsegmentEnd = fragments[j].endOffset
		}

		info := fragment.referenceTrafInfo
		references = append(references, SidxReference{
			ReferenceType:      false,
			ReferencedSize:     uint32(subsegmentEnd - fragments[subsegmentStart].moofInfo.Offset),
			SubsegmentDuration: info.duration,
			StartsWithSAP:      info.startsWithSAP,
			SAPType:            info.sapType,
			SAPDeltaTime:       info.sapDeltaTime,
		})
		// The next reference fragment starts a new sub-segment.
		subsegmentStart = -1
	}

	sidx := &Sidx{
		ReferenceID:    refTrack.trackID,
		Timescale:      refTrack.timescale,
		ReferenceCount: uint16(len(references)),
		References:     references,
	}
	if useVersion1 {
		sidx.SetVersion(1)
		sidx.EarliestPresentationTimeV1 = earliestPresentationTime
	} else {
		sidx.SetVersion(0)
		sidx.EarliestPresentationTimeV0 = uint32(earliestPresentationTime)
	}
	return sidx, nil
}

// sidxPresentationTime applies the first sample composition time offset to
// the base media decode time. Negative offsets are valid and must not
// underflow the unsigned presentation time.
func sidxPresentationTime(baseMediaDecodeTime uint64, firstCompositionTime int64) uint64 {
	if firstCompositionTime < 0 {
		delta := uint64(-firstCompositionTime)
		if delta >= baseMediaDecodeTime {
			return 0
		}
		return baseMediaDecodeTime - delta
	}
	return baseMediaDecodeTime + uint64(firstCompositionTime)
}

// probeSidxReferenceTraf locates the track fragment (traf) of trackID inside
// a moof box and derives its timing and SAP information from its real samples.
// It returns (nil, nil) when the moof box has no traf for the track.
func probeSidxReferenceTraf(r io.ReadSeeker, moof *BoxInfo, trackID uint32) (*sidxTrafInfo, error) {
	trafs, err := ExtractBox(r, moof, BoxPath{BoxTypeTraf()})
	if err != nil {
		return nil, err
	}

	for _, traf := range trafs {
		bips, err := ExtractBoxesWithPayload(r, traf, []BoxPath{
			{BoxTypeTfhd()},
			{BoxTypeTfdt()},
			{BoxTypeTrun()},
		})
		if err != nil {
			return nil, err
		}

		var tfhd *Tfhd
		var tfdt *Tfdt
		var trun *Trun
		for _, bip := range bips {
			switch bip.Info.Type {
			case BoxTypeTfhd():
				tfhd = bip.Payload.(*Tfhd)
			case BoxTypeTfdt():
				tfdt = bip.Payload.(*Tfdt)
			case BoxTypeTrun():
				trun = bip.Payload.(*Trun)
			}
		}
		if tfhd == nil {
			return nil, errors.New("tfhd box not found")
		}
		if tfhd.TrackID != trackID {
			continue
		}

		info := &sidxTrafInfo{
			sapType: 1,
		}
		if tfdt != nil {
			info.baseMediaDecodeTime = tfdt.GetBaseMediaDecodeTime()
		}
		if trun != nil {
			info.duration = sidxTrafDuration(tfhd, trun)
			info.firstCompositionTime = sidxTrafFirstCompositionTime(trun)
			info.startsWithSAP, info.sapType, info.sapDeltaTime = sidxTrafSAP(tfhd, trun)
		} else {
			info.startsWithSAP = true
		}
		return info, nil
	}
	return nil, nil
}

// sidxTrafDuration computes the total duration of the traf samples from the
// trun entries, falling back to the tfhd default sample duration.
func sidxTrafDuration(tfhd *Tfhd, trun *Trun) uint32 {
	if trun.CheckFlag(0x000100) {
		var duration uint32
		for i := range trun.Entries {
			duration += trun.Entries[i].SampleDuration
		}
		return duration
	}
	return tfhd.DefaultSampleDuration * trun.SampleCount
}

// sidxTrafFirstCompositionTime returns the composition time offset of the
// first sample, which is the earliest presentation time relative to the
// base media decode time within the fragment.
func sidxTrafFirstCompositionTime(trun *Trun) int64 {
	if trun.SampleCount == 0 || !trun.CheckFlag(0x000800) {
		return 0
	}
	return trun.GetSampleCompositionTimeOffset(0)
}

// sidxTrafSAP determines whether the fragment starts with a stream access
// point and derives the SAP type and SAP delta time from the real sample
// flags of the reference track. When the first sample is not a SAP, the
// first SAP following it is located inside the same fragment and its time
// is expressed as a delta from the first sample.
func sidxTrafSAP(tfhd *Tfhd, trun *Trun) (startsWithSAP bool, sapType, sapDeltaTime uint32) {
	hasPerSampleFlags := trun.CheckFlag(0x000400)
	defaultFlags := tfhd.DefaultSampleFlags
	// The first sample may use trun.first_sample_flags instead of
	// per-sample or tfhd default flags.
	if trun.CheckFlag(0x000004) && !hasPerSampleFlags {
		defaultFlags = trun.FirstSampleFlags
	}

	firstFlags := defaultFlags
	if hasPerSampleFlags && len(trun.Entries) > 0 {
		firstFlags = trun.Entries[0].SampleFlags
	}
	if sidxIsSyncSample(firstFlags) {
		return true, 1, 0
	}

	var decodeTime uint32
	durationFromTrun := trun.CheckFlag(0x000100)
	for i := range trun.Entries {
		flags := defaultFlags
		if hasPerSampleFlags {
			flags = trun.Entries[i].SampleFlags
		}
		if i > 0 && sidxIsSyncSample(flags) {
			return false, 1, decodeTime
		}
		if durationFromTrun {
			decodeTime += trun.Entries[i].SampleDuration
		} else {
			decodeTime += tfhd.DefaultSampleDuration
		}
	}
	return false, 0, 0
}

// sidxIsSyncSample reports whether a sample_flags value denotes a sync
// sample (a stream access point). The sample_depends_on field occupies
// bits 24-25 and value 2 means the sample does not depend on any other
// sample.
func sidxIsSyncSample(sampleFlags uint32) bool {
	return (sampleFlags>>24)&0x03 == 0x02
}

// sidxContainerBoxes are the boxes that may contain the leaf boxes whose
// absolute offsets have to be rewritten (tfra, mfro, stco or co64).
var sidxContainerBoxes = map[BoxType]bool{
	BoxTypeMoov(): true,
	BoxTypeTrak(): true,
	BoxTypeMdia(): true,
	BoxTypeMinf(): true,
	BoxTypeStbl(): true,
	BoxTypeMfra(): true,
}

// rewriteWithSidx streams the input file to the output, inserts the sidx box
// immediately before the first media fragment and updates every leaf box
// that keeps absolute file offsets.
func rewriteWithSidx(r io.ReadSeeker, w io.WriteSeeker, firstFragmentOffset uint64, sidx *Sidx) error {
	sidxBoxSize := SmallHeaderSize + sidxPayloadSize(sidx)
	writer := NewWriter(w)
	sidxInserted := false

	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		// The sidx box is inserted once, directly before the first root
		// level moof box, so first_offset can describe the real layout.
		if !sidxInserted && len(h.Path) == 1 && h.BoxInfo.Offset >= firstFragmentOffset {
			if err := writeSidxBox(writer, sidx, firstFragmentOffset, sidxBoxSize); err != nil {
				return nil, err
			}
			sidxInserted = true
		}

		switch h.BoxInfo.Type {
		case BoxTypeTfra():
			return nil, rewriteTfra(r, writer, &h.BoxInfo, sidxBoxSize)
		case BoxTypeMfro():
			return nil, rewriteMfro(r, writer, &h.BoxInfo, sidxBoxSize)
		case BoxTypeStco():
			return nil, rewriteStco(r, writer, &h.BoxInfo, sidxBoxSize)
		case BoxTypeCo64():
			return nil, rewriteCo64(r, writer, &h.BoxInfo, sidxBoxSize)
		case BoxTypeTfhd():
			return nil, rewriteTfhd(r, writer, &h.BoxInfo, sidxBoxSize)
		case BoxTypeMoof():
			// A tfhd box may carry an absolute base_data_offset. Only moof
			// boxes that do need such a fix are re-serialized; ordinary
			// fragments keep their original bytes untouched.
			needsFix, err := sidxMoofNeedsTfhdRewrite(r, &h.BoxInfo)
			if err != nil {
				return nil, err
			}
			if !needsFix {
				return nil, writer.CopyBox(r, &h.BoxInfo)
			}
			if _, err := writer.StartBox(&h.BoxInfo); err != nil {
				return nil, err
			}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			_, err = writer.EndBox()
			return nil, err
		case BoxTypeTraf():
			// Reached only when the parent moof had to be expanded for a
			// tfhd base_data_offset fix.
			if _, err := writer.StartBox(&h.BoxInfo); err != nil {
				return nil, err
			}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			_, err := writer.EndBox()
			return nil, err
		default:
			if sidxContainerBoxes[h.BoxInfo.Type] {
				// Re-emit the container header and descend, because the
				// offset boxes are located several levels below.
				if _, err := writer.StartBox(&h.BoxInfo); err != nil {
					return nil, err
				}
				if _, err := h.Expand(); err != nil {
					return nil, err
				}
				_, err := writer.EndBox()
				return nil, err
			}
			// All other boxes, including mdat and existing sidx boxes,
			// keep their payload byte for byte.
			return nil, writer.CopyBox(r, &h.BoxInfo)
		}
	})
	if err != nil {
		return err
	}
	if !sidxInserted {
		return errors.New("failed to insert sidx box")
	}
	return nil
}

// writeSidxBox writes one sidx box. FirstOffset is calculated from the
// actual output position so that it matches the real file layout.
func writeSidxBox(w *Writer, sidx *Sidx, oldFirstMoofOffset, sidxBoxSize uint64) error {
	current, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	// Everything before the insertion point is copied verbatim, therefore
	// the first moof box moves backward in the output by exactly sidxBoxSize.
	newFirstMoofOffset := oldFirstMoofOffset + sidxBoxSize
	firstOffset := newFirstMoofOffset - (uint64(current) + sidxBoxSize)

	if sidx.GetVersion() == 1 {
		sidx.FirstOffsetV1 = firstOffset
	} else {
		if firstOffset > uint64(^uint32(0)) {
			return errors.New("sidx first_offset exceeds uint32; use version 1")
		}
		sidx.FirstOffsetV0 = uint32(firstOffset)
	}

	if _, err := w.StartBox(&BoxInfo{Type: BoxTypeSidx()}); err != nil {
		return err
	}
	if _, err := Marshal(w, sidx, Context{}); err != nil {
		return err
	}
	_, err = w.EndBox()
	return err
}

// sidxPayloadSize returns the serialized payload size (excluding the box
// header) of a sidx box.
func sidxPayloadSize(sidx *Sidx) uint64 {
	// FullBox(4) + ReferenceID(4) + Timescale(4) +
	// EPT + FirstOffset + Reserved(2) + ReferenceCount(2)
	size := uint64(4 + 4 + 4 + 4 + 4 + 2 + 2)
	if sidx.GetVersion() == 1 {
		// EPT and FirstOffset are each 8 bytes instead of 4
		size += 8
	}
	// each reference is 12 bytes (96 bits)
	size += uint64(len(sidx.References)) * 12
	return size
}

// rewriteTfra shifts every absolute moof_offset by the inserted size.
func rewriteTfra(r io.ReadSeeker, w *Writer, bi *BoxInfo, shift uint64) error {
	box, err := readSidxPayload(r, bi)
	if err != nil {
		return err
	}
	tfra := box.(*Tfra)
	for i := range tfra.Entries {
		switch tfra.GetVersion() {
		case 0:
			tfra.Entries[i].MoofOffsetV0 += uint32(shift)
		case 1:
			tfra.Entries[i].MoofOffsetV1 += shift
		}
	}
	return writeSidxPayload(w, bi, tfra)
}

// rewriteMfro updates the mfra box size stored in the mfro box.
func rewriteMfro(r io.ReadSeeker, w *Writer, bi *BoxInfo, shift uint64) error {
	box, err := readSidxPayload(r, bi)
	if err != nil {
		return err
	}
	mfro := box.(*Mfro)
	mfro.Size += uint32(shift)
	return writeSidxPayload(w, bi, mfro)
}

// rewriteStco shifts every absolute chunk offset by the inserted size.
func rewriteStco(r io.ReadSeeker, w *Writer, bi *BoxInfo, shift uint64) error {
	box, err := readSidxPayload(r, bi)
	if err != nil {
		return err
	}
	stco := box.(*Stco)
	offsetShift := uint32(shift)
	for i := range stco.ChunkOffset {
		stco.ChunkOffset[i] += offsetShift
	}
	return writeSidxPayload(w, bi, stco)
}

// rewriteCo64 shifts every absolute chunk offset by the inserted size.
func rewriteCo64(r io.ReadSeeker, w *Writer, bi *BoxInfo, shift uint64) error {
	box, err := readSidxPayload(r, bi)
	if err != nil {
		return err
	}
	co64 := box.(*Co64)
	for i := range co64.ChunkOffset {
		co64.ChunkOffset[i] += shift
	}
	return writeSidxPayload(w, bi, co64)
}

// sidxMoofNeedsTfhdRewrite reports whether the moof box contains a tfhd box
// carrying an absolute base_data_offset, which must be shifted together with
// the other absolute offsets.
func sidxMoofNeedsTfhdRewrite(r io.ReadSeeker, moof *BoxInfo) (bool, error) {
	tfhdBips, err := ExtractBoxesWithPayload(r, moof, []BoxPath{{BoxTypeTraf(), BoxTypeTfhd()}})
	if err != nil {
		return false, err
	}
	for _, bip := range tfhdBips {
		if bip.Payload.(*Tfhd).CheckFlag(TfhdBaseDataOffsetPresent) {
			return true, nil
		}
	}
	return false, nil
}

// rewriteTfhd shifts the absolute base_data_offset when present. All other
// tfhd fields, including the default sample values, are kept unchanged.
func rewriteTfhd(r io.ReadSeeker, w *Writer, bi *BoxInfo, shift uint64) error {
	box, err := readSidxPayload(r, bi)
	if err != nil {
		return err
	}
	tfhd := box.(*Tfhd)
	if tfhd.CheckFlag(TfhdBaseDataOffsetPresent) {
		tfhd.BaseDataOffset += shift
	}
	return writeSidxPayload(w, bi, tfhd)
}

func readSidxPayload(r io.ReadSeeker, bi *BoxInfo) (IBox, error) {
	if _, err := bi.SeekToPayload(r); err != nil {
		return nil, err
	}
	if err := checkPayloadSize(r, bi); err != nil {
		return nil, err
	}
	box, _, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
	return box, err
}

func writeSidxPayload(w *Writer, bi *BoxInfo, box IImmutableBox) error {
	if _, err := w.StartBox(bi); err != nil {
		return err
	}
	if _, err := Marshal(w, box, bi.Context); err != nil {
		return err
	}
	_, err := w.EndBox()
	return err
}
