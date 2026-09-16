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

// encodeLine encodes one line string as WKB.
func encodeLine(bo binary.AppendByteOrder, pts []float64) []byte {
	var b []byte
	if bo == binary.BigEndian {
		b = append(b, 0)
	} else {
		b = append(b, 1)
	}
	b = bo.AppendUint32(b, wkbLineString)
	b = bo.AppendUint32(b, uint32(len(pts)/2))
	for _, v := range pts {
		b = bo.AppendUint64(b, math.Float64bits(v))
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
	for _, lat := range []float64{0, 0.015, -0.015, 45, 89.001, -89.001} {
		if d := Haversine(lat, 0, -lat, 180); math.IsNaN(d) || math.Abs(d-math.Pi*earthRadius) > 1 {
			t.Errorf("antipodes at latitude %g: distance %g, want %g", lat, d, math.Pi*earthRadius)
		}
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

// Endpoint reconstruction used to round 0.2 past its box, making Distance
// smaller than the pruning lower bound and Locate reject that same distance.
func TestLocateNearSegmentEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pts      []float64
		vertical bool
	}{
		{"forward", []float64{-0.1, 0, 0.2, 0}, false},
		{"reverse", []float64{0.2, 0, -0.1, 0}, false},
		{"vertical", []float64{0, -0.1, 0, 0.2}, true},
		{"vertical reverse", []float64{0, 0.2, 0, -0.1}, true},
		{"degenerate", []float64{0.2, 0, 0.2, 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Decode(encodeLine(binary.LittleEndian, tc.pts))
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []float64{0.2, 0.201, math.Nextafter(0.20015, 0), 0.20015, math.Nextafter(0.20015, math.Inf(1))} {
				x, y := q, 0.0
				if tc.vertical {
					x, y = y, x
				}
				want := q - 0.2
				d := g.Distance(x, y)
				if d != want {
					t.Errorf("(%g,%g): Distance = %.18g, want %.18g", x, y, d, want)
				}
				if _, bounded := g.Locate(x, y, d); bounded != d {
					t.Errorf("(%g,%g): Locate rejected reported distance %.18g: got %.18g", x, y, d, bounded)
				}
				wantIn := want <= Tolerance
				for _, bound := range []float64{math.Inf(1), want, 0} {
					wantD := want
					if want > math.Max(bound, Tolerance) {
						wantD = math.Inf(1)
					}
					if in, d := g.Locate(x, y, bound); in != wantIn || d != wantD {
						t.Errorf("(%g,%g) bound %.18g: Locate = %v %.18g, want %v %.18g", x, y, bound, in, d, wantIn, wantD)
					}
				}
			}
		})
	}
}

// scanRing measures every segment, closing the run when closed is set.
func scanRing(pts []float64, x, y float64, closed bool) float64 {
	n := len(pts) / 2
	switch n {
	case 0:
		return math.Inf(1)
	case 1:
		dx, dy := pts[0]-x, pts[1]-y
		return dx*dx + dy*dy
	}
	d := math.Inf(1)
	for i := 0; i+1 < n; i++ {
		d = math.Min(d, segmentDistance(pts[2*i], pts[2*i+1], pts[2*i+2], pts[2*i+3], x, y))
	}
	if closed {
		d = math.Min(d, segmentDistance(pts[2*n-2], pts[2*n-1], pts[0], pts[1], x, y))
	}
	return d
}

// fullScan checks containment and edge distance without pruning.
func fullScan(g *Geometry, x, y float64) (bool, float64) {
	in := false
	for _, poly := range g.polys {
		inside := false
		for _, r := range poly {
			n := len(r.pts) / 2
			odd := false
			for i, j := 0, n-1; i < n; j, i = i, i+1 {
				if crosses(r.pts[2*j], r.pts[2*j+1], r.pts[2*i], r.pts[2*i+1], x, y) {
					odd = !odd
				}
			}
			inside = inside != odd
		}
		in = in || inside
	}
	d := math.Inf(1)
	for _, poly := range g.polys {
		for _, r := range poly {
			d = math.Min(d, scanRing(r.pts, x, y, true))
		}
	}
	for _, l := range g.lines {
		d = math.Min(d, scanRing(l.pts, x, y, false))
	}
	for i := 0; i+1 < len(g.points); i += 2 {
		dx, dy := g.points[i]-x, g.points[i+1]-y
		d = math.Min(d, dx*dx+dy*dy)
	}
	return in, math.Sqrt(d)
}

func TestChunkBoundariesMatchFullScan(t *testing.T) {
	for _, n := range []int{0, 1, 2, chunkSize, chunkSize + 1, chunkSize + 2, 2 * chunkSize, 2*chunkSize + 1, 2*chunkSize + 2} {
		pts := circle(0, 0, 10, max(n, 1))[:2*n]
		// Leave the polygon unclosed to check its closing edge separately.
		for _, wkb := range [][]byte{encodeLine(binary.LittleEndian, pts), encodePolygon(binary.LittleEndian, pts)} {
			g, err := Decode(wkb)
			if err != nil {
				t.Fatal(err)
			}
			queries := [][2]float64{{0, 0}, {11, 1}, {-11, -1}}
			for i := 0; i < len(pts); i += 2 {
				queries = append(queries, [2]float64{pts[i], pts[i+1]}, [2]float64{pts[i] + 0.001, pts[i+1]})
			}
			for _, p := range queries {
				inside, edge := fullScan(g, p[0], p[1])
				if d := g.Distance(p[0], p[1]); d != edge {
					t.Fatalf("%d points, type %d, %v: Distance %g, want %g", n, wkb[1], p, d, edge)
				}
				want := edge
				if inside {
					want = 0
				}
				if in, d := g.Locate(p[0], p[1], edge); in != (inside || edge <= Tolerance) || d != want {
					t.Fatalf("%d points, type %d, %v: Locate %v %g, want %v %g", n, wkb[1], p, in, d, inside || edge <= Tolerance, want)
				}
			}
		}
	}
}

func TestChunkedScanMatchesFullScan(t *testing.T) {
	poly := encodePolygon(binary.LittleEndian, circle(0, 0, 10, 11*chunkSize), circle(2, 1, 3, 5*chunkSize))
	line := encodeLine(binary.BigEndian, circle(-20, 0, 4, 3*chunkSize))
	g, err := Decode(wkbMulti(binary.LittleEndian, wkbGeometryCollection, poly, line))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range append(slices.Clone(g.polys[0]), g.lines...) {
		if len(r.boxes) < 3 {
			t.Fatalf("%d points in %d chunks", len(r.pts)/2, len(r.boxes))
		}
	}
	// Sample inside and outside the rings and hole, away from vertices and edges.
	for x := -26.03; x < 13; x += 0.53 {
		for y := -12.07; y < 13; y += 0.61 {
			inside, edge := fullScan(g, x, y)
			wantIn, want := inside || edge <= Tolerance, edge
			if inside {
				want = 0
			}
			if in, d := g.Locate(x, y, math.Inf(1)); in != wantIn || !near(d, want) {
				t.Fatalf("(%g,%g): %v %.9f, want %v %.9f", x, y, in, d, wantIn, want)
			}
			if d := g.Distance(x, y); !near(d, edge) {
				t.Fatalf("(%g,%g) edge distance %.9f, want %.9f", x, y, d, edge)
			}
			if in, d := g.Locate(x, y, want); in != wantIn || !near(d, want) {
				t.Fatalf("(%g,%g) bound at the distance: %v %.9f, want %v %.9f", x, y, in, d, wantIn, want)
			}
			if in, d := g.Locate(x, y, 0.99*edge); !inside && edge > 2*Tolerance && (in || !math.IsInf(d, 1)) {
				t.Fatalf("(%g,%g) bound below the distance: %v %g", x, y, in, d)
			}
		}
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

// Use an unpruned reference; Distance also skips chunks.
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
		full := perOp(20, func() { _, d := fullScan(g, x, y); sink += d })
		ratios[i] = float64(full) / float64(bounded)
	}
	slices.Sort(ratios)
	if ratio := ratios[len(ratios)/2]; ratio < 20 {
		t.Errorf("full scan / bounded locate median ratio %.1fx, want at least 20x: the boxes are not skipping enough work", ratio)
	}
}

// Check every bearing; the largest longitude offset is poleward of the centre.
func TestWindowCoversTheDistance(t *testing.T) {
	for _, lat := range []float64{-90, -89.999, -89, -80, -60, -45, 0, 45, 60, 80, 89, 89.999, 90} {
		for _, m := range []float64{0, 100, 500, 2000, 100000, 2000000, math.Pi * earthRadius, 1e300} {
			w := Window(lat, m)
			if math.IsNaN(w) || w < 0 || w > 180 {
				t.Fatalf("lat %g, %g m: invalid window %g", lat, m, w)
			}
			phi := lat * math.Pi / 180
			a := math.Min(m/earthRadius, math.Pi)
			for bearing := 0; bearing < 360; bearing++ {
				b := float64(bearing) * math.Pi / 180
				sinLat := math.Sin(phi)*math.Cos(a) + math.Cos(phi)*math.Sin(a)*math.Cos(b)
				destLat := math.Asin(math.Max(-1, math.Min(1, sinLat)))
				destLon := math.Atan2(math.Sin(b)*math.Sin(a)*math.Cos(phi), math.Cos(a)-math.Sin(phi)*math.Sin(destLat))
				dy := math.Abs(destLat*180/math.Pi - lat)
				dx := math.Abs(destLon * 180 / math.Pi)
				if dx > w || dy > w {
					t.Fatalf("lat %g, %g m, bearing %d: offsets (%g, %g) outside square %g", lat, m, bearing, dx, dy, w)
				}
			}
		}
	}
	if got := Window(0, Haversine(0, 0, 0, 1)); math.Abs(got-1) > 2e-9 {
		t.Errorf("one degree at the equator: %g", got)
	}
	for _, lat := range []float64{-90, -89.999, 89.999, 90} {
		if got := Window(lat, 500); got != 180 {
			t.Errorf("circle reaching pole at %g: %g", lat, got)
		}
	}
}
