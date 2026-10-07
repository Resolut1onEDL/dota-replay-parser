package manta

import "testing"

func fixed8Field(varName, varType string, model int) *field {
	f := &field{varName: varName, varType: varType, encoder: "fixed8", fieldType: newFieldType(varType)}
	f.setModel(model)
	return f
}

func bitsRead(r *reader) uint32 {
	return r.pos*8 - r.bitCount
}

// CParticleSystem baseline of match 9032897977: m_iServerControlPointAssignments
// (uint8[4], fixed8) holds four raw 0xFF bytes, then m_hControlPointEnts.0000
// is the varint invalid handle 0xFFFFFF. Read as varints, the first 0xFF ate
// five bytes and the baseline ran out of buffer at m_hControlPointEnts.0062.
func TestFixed8ArrayOfUint8(t *testing.T) {
	f := fixed8Field("m_iServerControlPointAssignments", "uint8[4]", fieldModelFixedArray)
	r := newReader([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x07})
	for i := 0; i < 4; i++ {
		if got := f.decoder(r); got != uint64(255) {
			t.Fatalf("assignment %d = %#v, want uint64(255)", i, got)
		}
	}
	if got := unsignedDecoder(r); got != uint64(0xffffff) {
		t.Fatalf("handle after the array = %#v, want 0xffffff", got)
	}
	if n := bitsRead(r); n != 64 {
		t.Fatalf("read %d bits, want all 64", n)
	}
}

func TestFixed8KeepsValueTypes(t *testing.T) {
	cases := []struct {
		varName, varType string
		in               byte
		want             interface{}
	}{
		{"m_iCurrentMaxRagdollCount", "int8", 0xff, int32(-1)},
		{"m_nViewerType", "int8", 0x29, int32(41)},
		{"m_iTeamNum", "uint8", 0x83, uint64(131)},
		{"m_nAnimationAlgorithm", "AnimationAlgorithm_t", 0xff, uint32(255)},
	}
	for _, c := range cases {
		f := fixed8Field(c.varName, c.varType, fieldModelSimple)
		r := newReader([]byte{c.in, 0x01})
		if got := f.decoder(r); got != c.want {
			t.Errorf("%s (%s) = %#v, want %#v", c.varName, c.varType, got, c.want)
		}
		if n := bitsRead(r); n != 8 {
			t.Errorf("%s read %d bits, want 8", c.varName, n)
		}
	}
}

// CNetworkUtlVectorBase< uint8 > with fixed8: the length stays a varint, the
// elements are 8 raw bits.
func TestFixed8VectorElements(t *testing.T) {
	f := fixed8Field("m_vecPlayerDraftPickOrder", "CNetworkUtlVectorBase< uint8 >", fieldModelVariableArray)
	r := newReader([]byte{0x02, 0xff, 0x80})
	if got := f.baseDecoder(r); got != uint64(2) {
		t.Fatalf("length = %#v, want uint64(2)", got)
	}
	for i, want := range []uint64{255, 128} {
		if got := f.childDecoder(r); got != want {
			t.Fatalf("element %d = %#v, want uint64(%d)", i, got, want)
		}
	}
	if n := bitsRead(r); n != 24 {
		t.Fatalf("read %d bits, want all 24", n)
	}
}
