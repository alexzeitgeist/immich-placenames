package geo

import (
	"encoding/binary"
	"math"
	"testing"
)

// encodePolygon encodes rings as WKB in the given byte order.
func encodePolygon(bo binary.AppendByteOrder, rings ...[]float64) []byte {
	var b []byte
	if bo == binary.BigEndian {
		b = append(b, 0)
	} else {
		b = append(b, 1)
	}
	b = bo.AppendUint32(b, wkbPolygon)
	b = bo.AppendUint32(b, uint32(len(rings)))
	for _, r := range rings {
		b = bo.AppendUint32(b, uint32(len(r)/2))
		for _, v := range r {
			b = bo.AppendUint64(b, math.Float64bits(v))
		}
	}
	return b
}

func wkbMulti(bo binary.AppendByteOrder, t uint32, parts ...[]byte) []byte {
	var b []byte
	if bo == binary.BigEndian {
		b = append(b, 0)
	} else {
		b = append(b, 1)
	}
	b = bo.AppendUint32(b, t)
	b = bo.AppendUint32(b, uint32(len(parts)))
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

var (
	outer = []float64{0, 0, 10, 0, 10, 10, 0, 10, 0, 0}
	hole  = []float64{4, 4, 6, 4, 6, 6, 4, 6, 4, 4}
)

func TestSquareWithHole(t *testing.T) {
	for _, bo := range []binary.AppendByteOrder{binary.LittleEndian, binary.BigEndian} {
		g, err := Decode(encodePolygon(bo, outer, hole))
		if err != nil {
			t.Fatal(err)
		}
		if g.Bbox() != (Bbox{0, 0, 10, 10}) {
			t.Errorf("bbox %+v", g.Bbox())
		}
		cases := []struct {
			x, y float64
			want bool
			name string
		}{
			{2, 2, true, "inside"},
			{5, 5, false, "in hole"},
			{12, 12, false, "outside"},
			{10, 5, true, "on edge"},
			{10.00009, 5, true, "10 m outside, within tolerance"},
			{10.0003, 5, false, "30 m outside"},
			{4, 5, true, "on hole edge"},
			{5, 4.0001, true, "in hole, 10 m from its edge, within tolerance"},
		}
		for _, c := range cases {
			if got := g.Contains(c.x, c.y); got != c.want {
				t.Errorf("%v %s (%g,%g): got %v want %v", bo, c.name, c.x, c.y, got, c.want)
			}
		}
	}
}

// A ring the encoder left unclosed is the same shape for containment and
// for distance.
func TestUnclosedRing(t *testing.T) {
	g, err := Decode(encodePolygon(binary.LittleEndian, outer[:len(outer)-2]))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Contains(5, 5) || g.Contains(-0.5, 5) {
		t.Error("containment across the closing edge")
	}
	if d := g.Distance(-0.5, 5); d != 0.5 {
		t.Errorf("distance to the closing edge %g", d)
	}
	if !g.Contains(-0.0001, 5) {
		t.Error("10 m outside the closing edge, within tolerance")
	}
}

func TestMultiPolygonAndCollection(t *testing.T) {
	a := encodePolygon(binary.LittleEndian, outer)
	b := encodePolygon(binary.BigEndian, []float64{20, 20, 30, 20, 30, 30, 20, 30, 20, 20})
	g, err := Decode(wkbMulti(binary.LittleEndian, wkbMultiPolygon, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if !g.Contains(25, 25) || !g.Contains(5, 5) || g.Contains(15, 15) {
		t.Error("multipolygon containment")
	}
	if g.Bbox() != (Bbox{0, 0, 30, 30}) {
		t.Errorf("bbox %+v", g.Bbox())
	}
	col := wkbMulti(binary.BigEndian, wkbGeometryCollection, wkbMulti(binary.LittleEndian, wkbMultiPolygon, a), b)
	if g, err := Decode(col); err != nil || !g.Contains(25, 25) || !g.Contains(1, 1) {
		t.Errorf("collection: %v", err)
	}
}

func TestPointAndLine(t *testing.T) {
	var pt []byte
	pt = append(pt, 1)
	pt = binary.LittleEndian.AppendUint32(pt, wkbPoint)
	pt = binary.LittleEndian.AppendUint64(pt, math.Float64bits(1))
	pt = binary.LittleEndian.AppendUint64(pt, math.Float64bits(2))
	g, err := Decode(pt)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Contains(1.0001, 2) || g.Contains(1.001, 2) {
		t.Error("point tolerance")
	}
	var ln []byte
	ln = append(ln, 0)
	ln = binary.BigEndian.AppendUint32(ln, wkbLineString)
	ln = binary.BigEndian.AppendUint32(ln, 2)
	for _, v := range []float64{0, 0, 10, 0} {
		ln = binary.BigEndian.AppendUint64(ln, math.Float64bits(v))
	}
	if g, err := Decode(ln); err != nil || !g.Contains(5, 0.0001) || g.Contains(5, 1) {
		t.Errorf("line: %v", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, err := Decode(encodePolygon(binary.LittleEndian, outer)[:20]); err == nil {
		t.Error("truncated accepted")
	}
	if _, err := Decode(append(encodePolygon(binary.LittleEndian, outer), 0)); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := Decode([]byte{2, 0, 0, 0, 0}); err == nil {
		t.Error("bad byte order accepted")
	}
}

func TestHaversine(t *testing.T) {
	if d := Haversine(0, 0, 0, 1); math.Abs(d-111195) > 10 {
		t.Errorf("one degree at the equator: %f", d)
	}
}

func TestLocate(t *testing.T) {
	g, err := Decode(encodePolygon(binary.LittleEndian, outer, hole))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		x, y float64
		in   bool
		d    float64
	}{
		{2, 2, true, 0},
		{5, 5, false, 1},
		{10.0001, 5, true, 0.0001},
		{10.005, 5, false, 0.005},
		{13, 14, false, 5},
	}
	for _, c := range cases {
		if in, d := g.Locate(c.x, c.y); in != c.in || math.Abs(d-c.d) > 1e-9 {
			t.Errorf("(%g,%g): got %v %.6f, want %v %.6f", c.x, c.y, in, d, c.in, c.d)
		}
	}
	if in, d := (&Geometry{empty: true}).Locate(0, 0); in || !math.IsInf(d, 1) {
		t.Errorf("empty: %v %v", in, d)
	}
}
