// Package decoder implements an AAC decoder pipeline.
//
// This is a direct port of the AAC decoder module from AAC.js by Devon Govett
// (LGPL v3).
package decoder

import (
	"errors"
	"fmt"

	"github.com/skrashevich/go-aac/pkg/adts"
	"github.com/skrashevich/go-aac/pkg/cce"
	"github.com/skrashevich/go-aac/pkg/cpe"
	"github.com/skrashevich/go-aac/pkg/filterbank"
	"github.com/skrashevich/go-aac/pkg/ics"
	"github.com/skrashevich/go-aac/pkg/tables"
)

// ErrInvalidFrame identifies an error caused by a corrupt or undecodable
// frame. Unlike a configuration error, it describes a single frame only:
// the decoder keeps its configuration and filter state intact, so the caller
// can skip the offending frame and keep decoding the following ones.
//
// Use errors.Is to test for it:
//
//	samples, err := dec.DecodeFrame(frame)
//	if errors.Is(err, decoder.ErrInvalidFrame) {
//		// skip this frame, continue with the next one
//	}
var ErrInvalidFrame = errors.New("decoder: invalid frame")

// configError marks a fatal decoder configuration problem (unsupported
// profile, invalid channel configuration, etc.). Such problems are latched by
// the decoder: once one occurs, every subsequent DecodeFrame call fails with
// the same error, so skipping a frame can never make the decoder silently
// recover into an inconsistent state.
type configError struct {
	err error
}

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

func newConfigError(err error) error {
	if err == nil {
		return nil
	}
	return &configError{err: err}
}

// invalidFrame wraps err so that errors.Is(err, ErrInvalidFrame) reports true.
func invalidFrame(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalidFrame) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrInvalidFrame, err)
}

const (
	aotAACMain = 1
	aotAACLC   = 2
	aotAACLTP  = 4
	aotEscape  = 31
)

const (
	channelConfigNone               = 0
	channelConfigMono               = 1
	channelConfigStereo             = 2
	channelConfigStereoPlusCenter   = 3
	channelConfigStereoPlusRearMono = 4
	channelConfigFive               = 5
	channelConfigFivePlusOne        = 6
	channelConfigSevenPlusOne       = 8
)

const (
	sceElement = 0
	cpeElement = 1
	cceElement = 2
	lfeElement = 3
	dseElement = 4
	pceElement = 5
	filElement = 6
	endElement = 7
)

// Config contains the AAC decoder configuration.
type Config struct {
	Profile                int
	SampleIndex            int
	SampleRate             int
	ChanConfig             int
	FrameLength            int
	SectionDataResilience  bool
	ScalefactorResilience  bool
	SpectralDataResilience bool
}

// Decoder is a high-level AAC decoder.
type Decoder struct {
	Config     Config
	FilterBank *filterbank.FilterBank
	CCEs       []*cce.Element
	Data       [][]float32
	SBRPresent bool

	// configured reports whether SetASC or an ADTS header initialized the
	// decoder. It is kept separate from the Config fields because sample
	// index 0 (96 kHz) is a legitimate value.
	configured bool
	// configErr holds a latched configuration error. Once set, every call to
	// DecodeFrame fails with it.
	configErr error
}

// New creates a new AAC decoder.
func New() *Decoder {
	return &Decoder{}
}

// SetASC parses an MPEG-4 AudioSpecificConfig (2 bytes) and initializes the decoder.
//
// Invalid or unsupported configuration is a fatal, latched error: the decoder
// refuses all further DecodeFrame calls until a successful SetASC.
func (d *Decoder) SetASC(data []byte) error {
	stream := NewBitstream(data)
	config := Config{}

	config.Profile = int(stream.ReadBits(5))
	if config.Profile == aotEscape {
		config.Profile = 32 + int(stream.ReadBits(6))
	}

	config.SampleIndex = int(stream.ReadBits(4))
	if config.SampleIndex == 0x0f {
		config.SampleRate = int(stream.ReadBits(24))
		for i := 0; i < len(tables.SampleRates); i++ {
			if int(tables.SampleRates[i]) == config.SampleRate {
				config.SampleIndex = i
				break
			}
		}
	} else {
		if config.SampleIndex < 0 || config.SampleIndex >= len(tables.SampleRates) {
			return d.latchConfigError(fmt.Errorf("decoder: invalid sample index %d", config.SampleIndex))
		}
		config.SampleRate = int(tables.SampleRates[config.SampleIndex])
	}

	config.ChanConfig = int(stream.ReadBits(4))

	switch config.Profile {
	case aotAACMain, aotAACLC, aotAACLTP:
		if stream.ReadBits(1) != 0 {
			return d.latchConfigError(fmt.Errorf("decoder: frameLengthFlag not supported"))
		}
		config.FrameLength = 1024

		if stream.ReadBits(1) != 0 {
			_ = stream.ReadBits(14)
		}

		if stream.ReadBits(1) != 0 {
			if config.Profile > 16 {
				config.SectionDataResilience = stream.ReadBits(1) != 0
				config.ScalefactorResilience = stream.ReadBits(1) != 0
				config.SpectralDataResilience = stream.ReadBits(1) != 0
			}
			_ = stream.ReadBits(1)
		}

		if config.ChanConfig == channelConfigNone {
			_ = stream.ReadBits(4)
			return d.latchConfigError(fmt.Errorf("decoder: PCE unimplemented"))
		}
	default:
		return d.latchConfigError(fmt.Errorf("decoder: AAC profile %d not supported", config.Profile))
	}

	if err := stream.Error(); err != nil {
		return d.latchConfigError(err)
	}

	filterBank, err := filterbank.New(false, config.ChanConfig)
	if err != nil {
		return d.latchConfigError(err)
	}

	d.Config = config
	d.FilterBank = filterBank
	d.configured = true
	d.configErr = nil
	return nil
}

// DecodeFrame decodes a single AAC frame and returns interleaved PCM samples.
//
// A frame that cannot be decoded because of its own contents returns an error
// matching ErrInvalidFrame. The decoder's configuration and filter state are
// left untouched, so the caller may skip that frame and keep decoding the
// frames that follow.
//
// A configuration problem (unsupported profile, invalid channel
// configuration, ...) is fatal and latched: the returned error does not match
// ErrInvalidFrame, and every subsequent call fails with the same error.
func (d *Decoder) DecodeFrame(data []byte) ([]float32, error) {
	if d.configErr != nil {
		return nil, d.configErr
	}

	// Auto-configuration from an ADTS header is tentative: it only takes effect
	// if the frame decodes. Otherwise a corrupt first frame would lock in its
	// (possibly bogus) sample rate and channel count for the whole stream, and
	// the WAV header would describe the bad frame rather than the real audio.
	prevConfig := d.Config
	prevFilterBank := d.FilterBank
	prevConfigured := d.configured

	out, err := d.decodeFrame(data)
	if err != nil && !prevConfigured {
		d.Config = prevConfig
		d.FilterBank = prevFilterBank
		d.configured = prevConfigured
		d.CCEs = nil
		d.Data = nil
	}
	return out, err
}

// decodeFrame performs the actual per-frame decode. Configuration changes it
// makes are rolled back by DecodeFrame when the frame fails.
func (d *Decoder) decodeFrame(data []byte) ([]float32, error) {
	stream := NewBitstream(data)
	if stream.PeekBits(12) == 0xfff {
		header, err := adts.ReadHeaderFromBytes(data)
		if err != nil {
			return nil, invalidFrame(err)
		}
		// Never read past the frame declared in the ADTS header. A frame
		// length that does not describe a plausible frame within the supplied
		// bytes is treated as a corrupt frame rather than a stream error.
		if err := validateADTSFrameLength(header, len(data)); err != nil {
			return nil, err
		}
		stream = NewBitstream(data[:header.FrameLength])

		if !d.configured {
			if err := d.setConfigFromADTS(header); err != nil {
				return nil, d.latchConfigError(err)
			}
		}
		headerBits := 56
		if !header.ProtectionAbsent {
			headerBits = 72
		}
		stream.Advance(headerBits)
	}

	if d.FilterBank == nil || d.Config.FrameLength == 0 {
		// Not a latched configuration error: the frame simply cannot be
		// interpreted yet. A later ADTS frame may still configure the decoder.
		return nil, fmt.Errorf("decoder: config not initialized")
	}

	config := d.Config
	frameLength := config.FrameLength

	d.CCEs = nil
	elements := make([]frameElement, 0, 8)

	for elementType := int(stream.ReadBits(3)); elementType != endElement; elementType = int(stream.ReadBits(3)) {
		if err := stream.Error(); err != nil {
			return nil, invalidFrame(err)
		}
		id := int(stream.ReadBits(4))
		switch elementType {
		case sceElement, lfeElement:
			icsStream, err := ics.New(d.icsConfig())
			if err != nil {
				return nil, d.latchConfigError(err)
			}
			if err := icsStream.Decode(stream, d.icsConfig(), false); err != nil {
				return nil, invalidFrame(err)
			}
			elements = append(elements, frameElement{kind: elemSCE, id: id, sce: icsStream})
		case cpeElement:
			cpeElem, err := cpe.New(d.icsConfig())
			if err != nil {
				return nil, d.latchConfigError(err)
			}
			if err := cpeElem.Decode(stream, d.icsConfig()); err != nil {
				return nil, invalidFrame(err)
			}
			elements = append(elements, frameElement{kind: elemCPE, id: id, cpe: cpeElem})
		case cceElement:
			cceElem, err := cce.New(d.icsConfig())
			if err != nil {
				return nil, d.latchConfigError(err)
			}
			if err := cceElem.Decode(stream, d.icsConfig()); err != nil {
				return nil, invalidFrame(err)
			}
			d.CCEs = append(d.CCEs, cceElem)
		case dseElement:
			align := stream.ReadBits(1)
			count := int(stream.ReadBits(8))
			if count == 255 {
				count += int(stream.ReadBits(8))
			}
			if align != 0 {
				stream.Align()
			}
			stream.Advance(count * 8)
		case pceElement:
			return nil, invalidFrame(fmt.Errorf("decoder: PCE_ELEMENT not implemented"))
		case filElement:
			if id == 15 {
				id += int(stream.ReadBits(8)) - 1
			}
			stream.Advance(id * 8)
		default:
			return nil, invalidFrame(fmt.Errorf("decoder: unknown element type %d", elementType))
		}
	}

	stream.Align()
	if err := stream.Error(); err != nil {
		return nil, invalidFrame(err)
	}

	// Guard the overlap-add state: a frame that fails during processing must
	// not leave its partially written overlap data behind for the next frame.
	var snapshot [][]float32
	if d.FilterBank != nil {
		snapshot = d.FilterBank.Snapshot()
	}
	if err := d.process(elements); err != nil {
		d.FilterBank.Restore(snapshot)
		return nil, d.classifyProcessError(err)
	}

	channels := len(d.Data)
	output := make([]float32, frameLength*channels)
	idx := 0
	for k := 0; k < frameLength; k++ {
		for i := 0; i < channels; i++ {
			output[idx] = d.Data[i][k] / 32768.0
			idx++
		}
	}
	return output, nil
}

// validateADTSFrameLength checks that the frame length declared in the ADTS
// header describes a complete frame inside the supplied bytes.
func validateADTSFrameLength(header adts.Header, available int) error {
	minLength := 7
	if !header.ProtectionAbsent {
		minLength = 9
	}
	if header.FrameLength < minLength {
		return invalidFrame(fmt.Errorf("decoder: invalid ADTS frame length %d (minimum %d)", header.FrameLength, minLength))
	}
	if header.FrameLength > available {
		return invalidFrame(fmt.Errorf("decoder: ADTS frame length %d exceeds available %d bytes", header.FrameLength, available))
	}
	return nil
}

// classifyProcessError decides whether an error from the synthesis stage is a
// frame problem or a configuration problem, and latches the latter.
func (d *Decoder) classifyProcessError(err error) error {
	if err == nil {
		return nil
	}
	var ce *configError
	if errors.As(err, &ce) {
		d.configErr = err
		return err
	}
	return invalidFrame(err)
}

// latchConfigError records err as the decoder's permanent configuration error
// and returns it.
func (d *Decoder) latchConfigError(err error) error {
	if err == nil {
		return nil
	}
	var ce *configError
	if !errors.As(err, &ce) {
		err = newConfigError(err)
	}
	d.configErr = err
	return err
}

type elementKind int

const (
	elemSCE elementKind = iota
	elemCPE
)

type frameElement struct {
	kind elementKind
	id   int
	sce  *ics.ICStream
	cpe  *cpe.Element
}

func (d *Decoder) process(elements []frameElement) error {
	channels := d.Config.ChanConfig

	length := d.Config.FrameLength
	d.Data = make([][]float32, channels)
	for i := 0; i < channels; i++ {
		d.Data[i] = make([]float32, length)
	}

	channel := 0
	for i := 0; i < len(elements) && channel < channels; i++ {
		e := elements[i]
		switch e.kind {
		case elemSCE:
			count, err := d.processSingle(e.id, e.sce, channel)
			if err != nil {
				return err
			}
			channel += count
		case elemCPE:
			// A channel pair needs two free output channels. A corrupt frame in
			// a mono (or odd-channel) stream can contain a CPE and must be
			// rejected instead of writing past d.Data.
			if channel+1 >= channels {
				return fmt.Errorf("decoder: channel pair exceeds configured channels: %w", ErrInvalidFrame)
			}
			if err := d.processPair(e.id, e.cpe, channel); err != nil {
				return err
			}
			channel += 2
		default:
			return fmt.Errorf("decoder: unknown element kind")
		}
	}

	return nil
}

func (d *Decoder) processSingle(id int, element *ics.ICStream, channel int) (int, error) {
	profile := d.Config.Profile
	info := element.Info
	data := element.Data

	if profile == aotAACMain {
		return 0, newConfigError(fmt.Errorf("decoder: main prediction unimplemented"))
	}
	if profile == aotAACLTP {
		return 0, newConfigError(fmt.Errorf("decoder: LTP prediction unimplemented"))
	}

	if err := d.applyChannelCoupling(id, false, cce.BeforeTNS, data, nil); err != nil {
		return 0, err
	}

	if element.TnsPresent {
		element.ApplyTNS(data, false)
	}

	if err := d.applyChannelCoupling(id, false, cce.AfterTNS, data, nil); err != nil {
		return 0, err
	}

	if err := d.FilterBank.Process(windowInfo(info), data, d.Data[channel], channel); err != nil {
		return 0, err
	}

	if profile == aotAACLTP {
		return 0, fmt.Errorf("decoder: LTP prediction unimplemented")
	}

	if err := d.applyChannelCoupling(id, false, cce.AfterIMDCT, d.Data[channel], nil); err != nil {
		return 0, err
	}

	if element.GainPresent {
		return 0, fmt.Errorf("decoder: gain control not implemented")
	}
	if d.SBRPresent {
		return 0, fmt.Errorf("decoder: SBR not implemented")
	}

	return 1, nil
}

func (d *Decoder) processPair(id int, element *cpe.Element, channel int) error {
	profile := d.Config.Profile
	left := element.Left
	right := element.Right
	lInfo := left.Info
	rInfo := right.Info
	lData := left.Data
	rData := right.Data

	if element.CommonWindow && element.MaskPresent {
		d.processMS(element, lData, rData)
	}

	if profile == aotAACMain {
		return newConfigError(fmt.Errorf("decoder: main prediction unimplemented"))
	}

	d.processIS(element, lData, rData)

	if profile == aotAACLTP {
		return newConfigError(fmt.Errorf("decoder: LTP prediction unimplemented"))
	}

	if err := d.applyChannelCoupling(id, true, cce.BeforeTNS, lData, rData); err != nil {
		return err
	}

	if left.TnsPresent {
		left.ApplyTNS(lData, false)
	}
	if right.TnsPresent {
		right.ApplyTNS(rData, false)
	}

	if err := d.applyChannelCoupling(id, true, cce.AfterTNS, lData, rData); err != nil {
		return err
	}

	if err := d.FilterBank.Process(windowInfo(lInfo), lData, d.Data[channel], channel); err != nil {
		return err
	}
	if err := d.FilterBank.Process(windowInfo(rInfo), rData, d.Data[channel+1], channel+1); err != nil {
		return err
	}

	if profile == aotAACLTP {
		return fmt.Errorf("decoder: LTP prediction unimplemented")
	}

	if err := d.applyChannelCoupling(id, true, cce.AfterIMDCT, d.Data[channel], d.Data[channel+1]); err != nil {
		return err
	}

	if left.GainPresent || right.GainPresent {
		return fmt.Errorf("decoder: gain control not implemented")
	}
	if d.SBRPresent {
		return fmt.Errorf("decoder: SBR not implemented")
	}

	return nil
}

func (d *Decoder) processIS(element *cpe.Element, left, right []float32) {
	icsRight := element.Right
	info := icsRight.Info
	offsets := info.SwbOffsets
	windowGroups := info.GroupCount
	maxSFB := info.MaxSFB
	bandTypes := icsRight.BandTypes
	sectEnd := icsRight.SectEnd
	scaleFactors := icsRight.ScaleFactors

	idx := 0
	groupOff := 0
	for g := 0; g < windowGroups; g++ {
		for i := 0; i < maxSFB; {
			end := sectEnd[idx]
			if bandTypes[idx] == ics.IntensityBT || bandTypes[idx] == ics.IntensityBT2 {
				for ; i < end; i, idx = i+1, idx+1 {
					c := float32(1)
					if bandTypes[idx] == ics.IntensityBT2 {
						c = -1
					}
					if element.MaskPresent {
						if element.MSUsed[idx] {
							c = -c
						}
					}
					scale := c * scaleFactors[idx]
					for w := 0; w < info.GroupLength[g]; w++ {
						off := groupOff + w*128 + offsets[i]
						length := offsets[i+1] - offsets[i]
						for j := 0; j < length; j++ {
							right[off+j] = left[off+j] * scale
						}
					}
				}
			} else {
				idx += end - i
				i = end
			}
		}
		groupOff += info.GroupLength[g] * 128
	}
}

func (d *Decoder) processMS(element *cpe.Element, left, right []float32) {
	icsLeft := element.Left
	info := icsLeft.Info
	offsets := info.SwbOffsets
	windowGroups := info.GroupCount
	maxSFB := info.MaxSFB
	sfbCBl := icsLeft.BandTypes
	sfbCBr := element.Right.BandTypes

	groupOff := 0
	idx := 0
	for g := 0; g < windowGroups; g++ {
		for i := 0; i < maxSFB; i, idx = i+1, idx+1 {
			if element.MSUsed[idx] && sfbCBl[idx] < ics.NoiseBT && sfbCBr[idx] < ics.NoiseBT {
				for w := 0; w < info.GroupLength[g]; w++ {
					off := groupOff + w*128 + offsets[i]
					for j := 0; j < offsets[i+1]-offsets[i]; j++ {
						t := left[off+j] - right[off+j]
						left[off+j] += right[off+j]
						right[off+j] = t
					}
				}
			}
		}
		groupOff += info.GroupLength[g] * 128
	}
}

func (d *Decoder) applyChannelCoupling(elementID int, isChannelPair bool, couplingPoint int, data1, data2 []float32) error {
	applyIndependent := couplingPoint == cce.AfterIMDCT
	for _, element := range d.CCEs {
		if element.CouplingPoint != couplingPoint {
			continue
		}
		index := 0
		for c := 0; c < element.CoupledCount; c++ {
			chSelect := element.ChSelect[c]
			if element.ChannelPair[c] == isChannelPair && element.IDSelect[c] == elementID {
				if chSelect != 1 {
					if applyIndependent {
						if err := element.ApplyIndependentCoupling(index, data1); err != nil {
							return err
						}
					} else {
						if err := element.ApplyDependentCoupling(index, data1); err != nil {
							return err
						}
					}
					if chSelect != 0 {
						index++
					}
				}
				if chSelect != 2 {
					if applyIndependent {
						if err := element.ApplyIndependentCoupling(index, data2); err != nil {
							return err
						}
					} else {
						if err := element.ApplyDependentCoupling(index, data2); err != nil {
							return err
						}
					}
					index++
				}
			} else {
				index += 1
				if chSelect == 3 {
					index++
				}
			}
		}
	}
	return nil
}

// setConfigFromADTS initializes the decoder from an ADTS header. Errors are
// configuration errors and are latched by the caller.
func (d *Decoder) setConfigFromADTS(header adts.Header) error {
	config := d.Config
	config.Profile = header.Profile
	config.SampleIndex = header.SamplingIndex
	if config.SampleIndex < 0 || config.SampleIndex >= len(tables.SampleRates) {
		return fmt.Errorf("decoder: invalid sample index %d", config.SampleIndex)
	}
	config.SampleRate = int(tables.SampleRates[config.SampleIndex])
	config.ChanConfig = header.ChannelConfig
	config.FrameLength = 1024

	if config.Profile != aotAACMain && config.Profile != aotAACLC && config.Profile != aotAACLTP {
		return fmt.Errorf("decoder: AAC profile %d not supported", config.Profile)
	}
	if config.ChanConfig == channelConfigNone {
		return fmt.Errorf("decoder: PCE unimplemented")
	}

	filterBank, err := filterbank.New(false, config.ChanConfig)
	if err != nil {
		return err
	}

	d.Config = config
	d.FilterBank = filterBank
	d.configured = true
	return nil
}

func (d *Decoder) icsConfig() ics.Config {
	return ics.Config{
		SampleIndex: d.Config.SampleIndex,
		FrameLength: d.Config.FrameLength,
		Profile:     d.Config.Profile,
	}
}

func windowInfo(info *ics.ICSInfo) filterbank.WindowInfo {
	return filterbank.WindowInfo{
		WindowSequence: info.WindowSequence,
		WindowShape:    info.WindowShape,
	}
}
