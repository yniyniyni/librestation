package wav

import (
	"encoding/binary"
	"testing"
)

// riff builds a WAV with the given format fields around data.
func riff(channels, rate, bits int, data []byte) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, uint16(channels))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate*channels*bits/8))
	b = binary.LittleEndian.AppendUint16(b, uint16(channels*bits/8))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	return append(b, data...)
}

func TestDecode24BitKeepsChannel0(t *testing.T) {
	data := []byte{
		0x56, 0x34, 0x12, 0xff, 0xff, 0xff, // 0x123456, right channel ignored
		0x00, 0x00, 0x80, 0x01, 0x00, 0x00, // the most negative value
	}
	pcm, rate, err := Decode(riff(2, 48000, 24, data))
	if err != nil {
		t.Fatal(err)
	}
	if rate != 48000 || len(pcm) != 2 || pcm[0] != 0x1234 || pcm[1] != -0x8000 {
		t.Errorf("rate %d, pcm %#v", rate, pcm)
	}
}

func TestDecode16Bit(t *testing.T) {
	pcm, rate, err := Decode(riff(1, 44100, 16, []byte{0x34, 0x12, 0xff, 0xff}))
	if err != nil {
		t.Fatal(err)
	}
	if rate != 44100 || len(pcm) != 2 || pcm[0] != 0x1234 || pcm[1] != -1 {
		t.Errorf("rate %d, pcm %#v", rate, pcm)
	}
}

func TestDecodeRefusesOtherDepths(t *testing.T) {
	if _, _, err := Decode(riff(1, 44100, 8, []byte{1, 2})); err == nil {
		t.Error("8 bits decoded")
	}
}
