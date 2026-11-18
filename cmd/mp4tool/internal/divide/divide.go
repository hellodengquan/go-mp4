package divide

import (
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"

	"github.com/abema/go-mp4"
)

const (
	videoDirName     = "video"
	audioDirName     = "audio"
	encVideoDirName  = "video_enc"
	encAudioDirName  = "audio_enc"
	initMP4FileName  = "init.mp4"
	playlistFileName = "playlist.m3u8"
)

func segmentFileName(i int) string {
	return fmt.Sprintf("%d.mp4", i)
}

func Main(args []string) int {
	if len(args) < 2 {
		println("USAGE: mp4tool divide INPUT.mp4 OUTPUT_DIR")
		return 1
	}

	if err := divide(args[0], args[1]); err != nil {
		fmt.Println("Error:", err)
		return 1
	}
	return 0
}

type childInfo map[uint32]uint64

type segment struct {
	duration float64
}

type trackType int

const (
	trackVideo trackType = iota
	trackAudio
	trackEncVideo
	trackEncAudio
)

type track struct {
	id          uint32
	trackType   trackType
	timescale   uint32
	bandwidth   uint64
	height      uint16
	width       uint16
	channels    uint16
	codec       string
	segments    []segment
	outputDir   string
	initFile    *os.File
	segmentFile *os.File
}

func divide(inputFilePath, outputDir string) error {
	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		return err
	}
	defer inputFile.Close()

	// probe the input once so that the report and the playlist share the same
	// interpretation of the codecs carried by the file
	probedTracks := make(map[uint32]*mp4.Track, 4)
	probeInfo, err := mp4.Probe(inputFile)
	if err != nil {
		// A damaged or incomplete media file must not abort the segmentation;
		// the codec strings are simply omitted from the playlist below.
		fmt.Printf("WARN: failed to probe codec information: %v\n", err)
	} else {
		for _, tr := range probeInfo.Tracks {
			probedTracks[tr.TrackID] = tr
		}
	}

	// generate track map
	tracks := make(map[uint32]*track, 4)
	bis, err := mp4.ExtractBox(inputFile, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		return err
	}
	for _, bi := range bis {
		t := new(track)

		// get trackID from Tkhd box
		bs, err := mp4.ExtractBoxWithPayload(inputFile, bi, mp4.BoxPath{mp4.BoxTypeTkhd()})
		if err != nil {
			return err
		} else if len(bs) != 1 {
			return fmt.Errorf("trak box must have one tkhd box: tkhd=%d", len(bs))
		}
		tkhd := bs[0].Payload.(*mp4.Tkhd)
		t.id = tkhd.TrackID

		// get timescale from Mdhd box
		bs, err = mp4.ExtractBoxWithPayload(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
		if err != nil {
			return err
		} else if len(bs) != 1 {
			return fmt.Errorf("trak box must have one mdhd box: mdhd=%d", len(bs))
		}
		mdhd := bs[0].Payload.(*mp4.Mdhd)
		t.timescale = mdhd.Timescale

		if pt, exists := probedTracks[t.id]; exists {
			t.codec = pt.CodecString()
			if pt.MP4A != nil {
				t.channels = pt.MP4A.ChannelCount
			}
		}

		bs, err = mp4.ExtractBoxWithPayload(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.StrToBoxType("avc1")})
		if err != nil {
			return err
		}
		if len(bs) != 0 {
			avc1 := bs[0].Payload.(*mp4.VisualSampleEntry)
			t.trackType = trackVideo
			t.height = avc1.Height
			t.width = avc1.Width
			t.outputDir = path.Join(outputDir, videoDirName)
			tracks[t.id] = t
			continue
		}

		// HEVC tracks use either the hvc1 or the hev1 sample entry
		bs, err = mp4.ExtractBoxWithPayload(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeHvc1()})
		if err != nil {
			return err
		}
		if len(bs) == 0 {
			bs, err = mp4.ExtractBoxWithPayload(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.BoxTypeHev1()})
			if err != nil {
				return err
			}
		}
		if len(bs) != 0 {
			hevc := bs[0].Payload.(*mp4.VisualSampleEntry)
			t.trackType = trackVideo
			t.height = hevc.Height
			t.width = hevc.Width
			t.outputDir = path.Join(outputDir, videoDirName)
			tracks[t.id] = t
			continue
		}

		bis, err = mp4.ExtractBox(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.StrToBoxType("mp4a")})
		if err != nil {
			return err
		}
		if len(bis) != 0 {
			t.trackType = trackAudio
			t.outputDir = path.Join(outputDir, audioDirName)
			tracks[t.id] = t
			continue
		}

		bs, err = mp4.ExtractBoxWithPayload(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.StrToBoxType("encv")})
		if err != nil {
			return err
		}
		if len(bs) != 0 {
			encv := bs[0].Payload.(*mp4.VisualSampleEntry)
			t.trackType = trackEncVideo
			t.height = encv.Height
			t.width = encv.Width
			t.outputDir = path.Join(outputDir, encVideoDirName)
			tracks[t.id] = t
			continue
		}

		bis, err = mp4.ExtractBox(inputFile, bi, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd(), mp4.StrToBoxType("enca")})
		if err != nil {
			return err
		}
		if len(bis) != 0 {
			t.trackType = trackEncAudio
			t.outputDir = path.Join(outputDir, encAudioDirName)
			tracks[t.id] = t
			continue
		}

		fmt.Printf("WARN: unsupported track type: trackID=%d\n", t.id)
	}

	for _, t := range tracks {
		os.MkdirAll(t.outputDir, 0777)

		if t.initFile, err = os.Create(path.Join(t.outputDir, initMP4FileName)); err != nil {
			return err
		}
		defer t.initFile.Close()

		if t.segmentFile, err = os.Create(path.Join(t.outputDir, segmentFileName(0))); err != nil {
			return err
		}
		defer func(t *track) { t.segmentFile.Close() }(t)
	}

	currTrackID := uint32(math.MaxUint32)
	if _, err = mp4.ReadBoxStructure(inputFile, func(h *mp4.ReadHandle) (interface{}, error) {
		// initialization segment
		if h.BoxInfo.Type == mp4.BoxTypeMoov() ||
			h.BoxInfo.Type == mp4.BoxTypeFtyp() ||
			h.BoxInfo.Type == mp4.BoxTypePssh() ||
			h.BoxInfo.Type == mp4.BoxTypeMvhd() ||
			h.BoxInfo.Type == mp4.BoxTypeTrak() ||
			h.BoxInfo.Type == mp4.BoxTypeMvex() ||
			h.BoxInfo.Type == mp4.BoxTypeUdta() ||
			h.BoxInfo.Type == mp4.BoxTypeSidx() {

			var writeAll bool
			var trackID uint32
			if h.BoxInfo.Type == mp4.BoxTypeTrak() {
				// get trackID from Tkhd box
				bs, err := mp4.ExtractBoxWithPayload(inputFile, &h.BoxInfo, mp4.BoxPath{mp4.BoxTypeTkhd()})
				if err != nil {
					return nil, err
				} else if len(bs) != 1 {
					return nil, fmt.Errorf("trak box must have one tkhd box: tkhd=%d", len(bs))
				}
				tkhd := bs[0].Payload.(*mp4.Tkhd)
				trackID = tkhd.TrackID

			} else {
				writeAll = true
			}

			offsetMap := make(map[uint32]int64, len(tracks))
			biMap := make(map[uint32]*mp4.BoxInfo, len(tracks))
			for _, t := range tracks {
				if writeAll || t.id == trackID {
					if offsetMap[t.id], err = t.initFile.Seek(0, io.SeekEnd); err != nil {
						return nil, err
					}
					if biMap[t.id], err = mp4.WriteBoxInfo(t.initFile, &h.BoxInfo); err != nil {
						return nil, err
					}
					biMap[t.id].Size = biMap[t.id].HeaderSize
				}
			}

			if h.BoxInfo.Type == mp4.BoxTypeMoov() {
				vals, err := h.Expand()
				if err != nil {
					return nil, err
				}
				for _, val := range vals {
					ci := val.(childInfo)
					for _, t := range tracks {
						// already writeAll is true in Moov box
						biMap[t.id].Size += ci[t.id]
					}
				}

			} else {
				// copy all data of payload
				for _, t := range tracks {
					if writeAll || t.id == trackID {
						n, err := h.ReadData(t.initFile)
						if err != nil {
							return nil, err
						}
						biMap[t.id].Size += uint64(n)
					}
				}
			}

			// rewrite headers
			for _, t := range tracks {
				if writeAll || t.id == trackID {
					if _, err = t.initFile.Seek(offsetMap[t.id], io.SeekStart); err != nil {
						return nil, err
					}
					if biMap[t.id], err = mp4.WriteBoxInfo(t.initFile, biMap[t.id]); err != nil {
						return nil, err
					}
				}
			}

			ci := make(childInfo, 0)
			for id, bi := range biMap {
				ci[id] = bi.Size
			}
			return ci, nil
		}

		// media segment
		if h.BoxInfo.Type == mp4.BoxTypeMoof() ||
			h.BoxInfo.Type == mp4.BoxTypeMdat() {

			if h.BoxInfo.Type == mp4.BoxTypeMoof() {
				// extract Tfdt-box
				bs, err := mp4.ExtractBoxWithPayload(inputFile, &h.BoxInfo, mp4.BoxPath{mp4.BoxTypeTraf(), mp4.BoxTypeTfhd()})
				if err != nil {
					return nil, err
				} else if len(bs) != 1 {
					return nil, fmt.Errorf("trak box must have one tkhd box: tkhd=%d", len(bs))
				}
				tfhd := bs[0].Payload.(*mp4.Tfhd)

				currTrackID = tfhd.TrackID
				if _, exists := tracks[currTrackID]; !exists {
					return nil, nil
				}

				var defaultSampleDuration uint32
				if tfhd.CheckFlag(0x000008) {
					defaultSampleDuration = tfhd.DefaultSampleDuration
				}

				// extract Trun-box
				bs, err = mp4.ExtractBoxWithPayload(inputFile, &h.BoxInfo, mp4.BoxPath{mp4.BoxTypeTraf(), mp4.BoxTypeTrun()})
				if err != nil {
					return nil, err
				}
				trun := bs[0].Payload.(*mp4.Trun)

				var duration uint32
				for i := range trun.Entries {
					if trun.CheckFlag(0x000100) {
						duration += trun.Entries[i].SampleDuration
					} else {
						duration += defaultSampleDuration
					}
				}

				// close last segment file and create next
				t := tracks[currTrackID]
				t.segmentFile.Close()
				if t.segmentFile, err = os.Create(path.Join(t.outputDir, segmentFileName(len(t.segments)))); err != nil {
					return nil, err
				}
				t.segments = append(t.segments, segment{
					duration: float64(duration) / float64(t.timescale),
				})

			} else { // Mdat box
				if _, exists := tracks[currTrackID]; !exists {
					return nil, nil
				}

				t := tracks[currTrackID]
				bandwidth := uint64(float64(h.BoxInfo.Size) * 8 / t.segments[len(t.segments)-1].duration)
				if bandwidth > t.bandwidth {
					t.bandwidth = bandwidth
				}
			}

			t := tracks[currTrackID]
			if _, err := mp4.WriteBoxInfo(t.segmentFile, &h.BoxInfo); err != nil {
				return nil, err
			}
			if _, err := h.ReadData(t.segmentFile); err != nil {
				return nil, err
			}

			return nil, nil
		}

		// skip
		if h.BoxInfo.Type == mp4.BoxTypeMfra() {
			return nil, nil
		}

		return nil, fmt.Errorf("unexpected box type: %s", h.BoxInfo.Type.String())
	}); err != nil {
		return err
	}

	trackTypeMap := make(map[trackType]*track, len(tracks))
	for _, t := range tracks {
		trackTypeMap[t.trackType] = t
	}

	if err := outputMasterPlaylist(path.Join(outputDir, playlistFileName), trackTypeMap); err != nil {
		return err
	}

	for _, t := range tracks {
		if err := outputMediaPlaylist(path.Join(t.outputDir, playlistFileName), t.segments); err != nil {
			return err
		}
	}

	return nil
}

func outputMasterPlaylist(filePath string, trackTypeMap map[trackType]*track) error {
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	var adir string
	var at *track
	var vdir string
	var vt *track

	if t, exists := trackTypeMap[trackAudio]; exists {
		adir = audioDirName
		at = t
	} else if t, exists := trackTypeMap[trackEncAudio]; exists {
		adir = encAudioDirName
		at = t
	}

	if t, exists := trackTypeMap[trackVideo]; exists {
		vdir = videoDirName
		vt = t
	} else if t, exists := trackTypeMap[trackEncVideo]; exists {
		vdir = encVideoDirName
		vt = t
	}

	file.WriteString("#EXTM3U\n")
	if adir != "" {
		file.WriteString("#EXT-X-MEDIA:TYPE=AUDIO,URI=\"" + adir + "/" + playlistFileName + "\",GROUP-ID=\"audio\",NAME=\"audio\",AUTOSELECT=YES")
		if at.channels != 0 {
			_, err = fmt.Fprintf(file, ",CHANNELS=\"%d\"", at.channels)
			if err != nil {
				return err
			}
		}
		file.WriteString("\n")
	}
	if vdir != "" {
		// CODECS carries the codec strings probed from the actual sample
		// entries.  A codec whose profile information could not be read is
		// left out instead of being advertised with a made up value.
		codecs := make([]string, 0, 2)
		if isCompleteCodecString(vt.codec) {
			codecs = append(codecs, vt.codec)
		}
		if at != nil && isCompleteCodecString(at.codec) {
			codecs = append(codecs, at.codec)
		}
		_, err = fmt.Fprintf(file, "#EXT-X-STREAM-INF:BANDWIDTH=%d", vt.bandwidth)
		if err != nil {
			return err
		}
		if len(codecs) != 0 {
			if _, err = fmt.Fprintf(file, ",CODECS=\"%s\"", strings.Join(codecs, ",")); err != nil {
				return err
			}
		}
		if vt.width != 0 && vt.height != 0 {
			if _, err = fmt.Fprintf(file, ",RESOLUTION=%dx%d", vt.width, vt.height); err != nil {
				return err
			}
		}
		if adir != "" {
			file.WriteString(",AUDIO=\"audio\"")
		}
		file.WriteString("\n")
		file.WriteString(vdir + "/" + playlistFileName + "\n")
	}
	return nil
}

// isCompleteCodecString reports whether a codec string carries the profile
// information required by HLS.  A bare sample entry name means that the
// decoder configuration was missing, and advertising it in CODECS would claim
// media attributes that were never read from the file.
func isCompleteCodecString(codec string) bool {
	return strings.Contains(codec, ".")
}

func outputMediaPlaylist(filePath string, segments []segment) error {
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	var maxDur float64
	for i := range segments {
		if segments[i].duration > maxDur {
			maxDur = segments[i].duration
		}
	}

	file.WriteString("#EXTM3U\n")
	file.WriteString("#EXT-X-VERSION:7\n")
	if _, err := fmt.Fprintf(file, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxDur))); err != nil {
		return err
	}
	file.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	file.WriteString("#EXT-X-MAP:URI=\"" + initMP4FileName + "\"\n")
	for i := range segments {
		if _, err := fmt.Fprintf(file, "#EXTINF:%f,\n", segments[i].duration); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(file, "%s\n", segmentFileName(i)); err != nil {
			return err
		}
	}
	file.WriteString("#EXT-X-ENDLIST\n")
	return nil
}
