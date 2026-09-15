package geo

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"time"
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

// near compares distances, +Inf included.
func near(a, b float64) bool { return a == b || math.Abs(a-b) <= 1e-9 }

func TestLocate(t *testing.T) {
	g, err := Decode(encodePolygon(binary.LittleEndian, outer, hole))
	if err != nil {
		t.Fatal(err)
	}
	inf := math.Inf(1)
	cases := []struct {
		x, y, bound float64
		in          bool
		d           float64
	}{
		{2, 2, inf, true, 0},
		{5, 5, inf, false, 1},
		{10.0001, 5, inf, true, 0.0001},
		{10.005, 5, inf, false, 0.005},
		{13, 14, inf, false, 5},
		// Bounds below Tolerance must still preserve containment.
		{2, 2, 0, true, 0},
		{10.0001, 5, 0, true, 0.0001},
		{10.005, 5, 0.0051, false, 0.005},
		{10.005, 5, 0.0049, false, inf},
		{13, 14, 1, false, inf},
		// Distances equal to the bound pass both cutoffs. The hole
		// tests only the distance cutoff, since its box distance is zero.
		{11, 5, 1, false, 1},
		{13, 14, 5, false, 5},
		{5, 5, 1, false, 1},
	}
	for _, c := range cases {
		if in, d := g.Locate(c.x, c.y, c.bound); in != c.in || !near(d, c.d) {
			t.Errorf("(%g,%g) bound %g: got %v %.6f, want %v %.6f", c.x, c.y, c.bound, in, d, c.in, c.d)
		}
	}
	if in, d := (&Geometry{empty: true}).Locate(0, 0, inf); in || !math.IsInf(d, 1) {
		t.Errorf("empty: %v %v", in, d)
	}
}

// The geometry box spans the antimeridian gap; the polygon boxes exclude it.
func TestPolygonBoxesSkipTheGap(t *testing.T) {
	west := encodePolygon(binary.LittleEndian, []float64{-180, -10, -170, -10, -170, 10, -180, 10, -180, -10})
	east := encodePolygon(binary.LittleEndian, []float64{170, -10, 180, -10, 180, 10, 170, 10, 170, -10})
	g, err := Decode(wkbMulti(binary.LittleEndian, wkbMultiPolygon, west, east))
	if err != nil {
		t.Fatal(err)
	}
	if g.Bbox() != (Bbox{-180, -10, 180, 10}) {
		t.Errorf("bbox %+v", g.Bbox())
	}
	if !g.Contains(-175, 0) || !g.Contains(175, 0) || g.Contains(0, 0) {
		t.Error("containment across the gap")
	}
	if d := g.Distance(0, 0); d != 170 {
		t.Errorf("distance across the gap %g, want 170", d)
	}
	if in, d := g.Locate(0, 0, 1); in || !math.IsInf(d, 1) {
		t.Errorf("bounded locate in the gap: %v %g", in, d)
	}
}

// circle returns a closed ring of n vertices of radius r around cx,cy.
func circle(cx, cy, r float64, n int) []float64 {
	ring := make([]float64, 0, 2*n+2)
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		ring = append(ring, cx+r*math.Cos(a), cy+r*math.Sin(a))
	}
	return append(ring, ring[0], ring[1])
}

var sink float64

func perOp(n int, f func()) time.Duration {
	start := time.Now()
	for range n {
		f()
	}
	return time.Since(start) / time.Duration(n)
}

// Compare bounded lookup with exact Distance, which also prunes polygons.
// The median reduces scheduling noise; the ratio allows for slower runners.
func TestBoundedLocateSkipsDistantPolygons(t *testing.T) {
	const polys, vertices = 128, 512
	parts := make([][]byte, 0, polys+1)
	for i := range polys {
		parts = append(parts, encodePolygon(binary.LittleEndian, circle(float64(i)*4, 0, 1, vertices)))
	}
	// One far polygon stretches the geometry's own box over the query point.
	parts = append(parts, encodePolygon(binary.LittleEndian, circle(256, 80, 1, vertices)))
	g, err := Decode(wkbMulti(binary.LittleEndian, wkbMultiPolygon, parts...))
	if err != nil {
		t.Fatal(err)
	}
	const x, y = 256, 40
	if !g.Bbox().Contains(x, y) {
		t.Fatalf("query outside the geometry box %+v", g.Bbox())
	}
	if in, d := g.Locate(x, y, Tolerance); in || !math.IsInf(d, 1) {
		t.Fatalf("locate %v %g, want no match", in, d)
	}
	if n := testing.AllocsPerRun(50, func() { _, d := g.Locate(x, y, Tolerance); sink += d }); n != 0 {
		t.Errorf("%v allocations per locate", n)
	}
	ratios := make([]float64, 5)
	for i := range ratios {
		bounded := perOp(5000, func() { _, d := g.Locate(x, y, Tolerance); sink += d })
		exact := perOp(20, func() { sink += g.Distance(x, y) })
		ratios[i] = float64(exact) / float64(bounded)
	}
	slices.Sort(ratios)
	if ratio := ratios[len(ratios)/2]; ratio < 20 {
		t.Errorf("exact distance / bounded locate median ratio %.1fx, want at least 20x: the polygon boxes are not skipping enough work", ratio)
	}
}
