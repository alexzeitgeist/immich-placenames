// Package geo decodes WKB and tests containment and distance in planar degrees.
package geo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Tolerance is the planar distance in degrees within which a point outside a
// geometry still counts as contained.
const Tolerance = 0.00015

// Bbox is a lon/lat bounding box.
type Bbox struct {
	XMin float64 `json:"xmin"`
	YMin float64 `json:"ymin"`
	XMax float64 `json:"xmax"`
	YMax float64 `json:"ymax"`
}

// Contains reports whether the point lies inside or on the box.
func (b Bbox) Contains(lon, lat float64) bool {
	return lon >= b.XMin && lon <= b.XMax && lat >= b.YMin && lat <= b.YMax
}

// Distance is the planar distance in degrees from the point to the box, zero inside.
func (b Bbox) Distance(lon, lat float64) float64 {
	dx := math.Max(0, math.Max(b.XMin-lon, lon-b.XMax))
	dy := math.Max(0, math.Max(b.YMin-lat, lat-b.YMax))
	return math.Hypot(dx, dy)
}

// Intersects reports whether the boxes overlap or touch.
func (b Bbox) Intersects(o Bbox) bool {
	return b.XMax >= o.XMin && b.XMin <= o.XMax && b.YMax >= o.YMin && b.YMin <= o.YMax
}

// Area is the box area in square degrees.
func (b Bbox) Area() float64 { return math.Abs((b.XMax - b.XMin) * (b.YMax - b.YMin)) }

// Center is the box centre.
func (b Bbox) Center() (lon, lat float64) { return (b.XMin + b.XMax) / 2, (b.YMin + b.YMax) / 2 }

// Union is the smallest box covering both.
func (b Bbox) Union(o Bbox) Bbox {
	return Bbox{math.Min(b.XMin, o.XMin), math.Min(b.YMin, o.YMin), math.Max(b.XMax, o.XMax), math.Max(b.YMax, o.YMax)}
}

// Expand grows the box by d on every side.
func (b Bbox) Expand(d float64) Bbox { return Bbox{b.XMin - d, b.YMin - d, b.XMax + d, b.YMax + d} }

// Geometry is a decoded WKB geometry: polygons with holes, lines and points.
type Geometry struct {
	polys  [][][]float64 // polygon → ring → x0,y0,x1,y1,...; the first ring is the outer one
	boxes  []Bbox        // one box per polygon
	lines  [][]float64
	points []float64
	bbox   Bbox
	empty  bool
}

// Bbox is the geometry's bounding box.
func (g *Geometry) Bbox() Bbox { return g.bbox }

// Contains reports whether the point lies inside a polygon, holes excluded,
// or within Tolerance of any edge or point.
func (g *Geometry) Contains(lon, lat float64) bool {
	if g.empty || !g.bbox.Expand(Tolerance).Contains(lon, lat) {
		return false
	}
	in, _ := g.Locate(lon, lat, Tolerance)
	return in
}

// Locate reports containment and planar distance in degrees: zero inside a
// polygon, +Inf for empty geometry or beyond max(bound, Tolerance).
// Containment always includes points within Tolerance of an edge or point.
func (g *Geometry) Locate(lon, lat, bound float64) (bool, float64) {
	if g.empty {
		return false, math.Inf(1)
	}
	for i, poly := range g.polys {
		if g.boxes[i].Contains(lon, lat) && polygonContains(poly, lon, lat) {
			return true, 0
		}
	}
	d := g.distanceWithin(lon, lat, math.Max(bound, Tolerance))
	return d <= Tolerance, d
}

// Distance is the planar distance in degrees from the point to the nearest
// edge or vertex of the geometry, or +Inf for an empty geometry.
func (g *Geometry) Distance(lon, lat float64) float64 {
	return g.distanceWithin(lon, lat, math.Inf(1))
}

// distanceWithin returns +Inf beyond bound. Polygon boxes give a lower bound
// on edge distance, so polygons beyond min(d, bound) can be skipped.
func (g *Geometry) distanceWithin(lon, lat, bound float64) float64 {
	d := math.Inf(1)
	for i, poly := range g.polys {
		if g.boxes[i].Distance(lon, lat) > math.Min(d, bound) {
			continue
		}
		for _, ring := range poly {
			d = math.Min(d, ringDistance(ring, lon, lat))
		}
	}
	for _, line := range g.lines {
		d = math.Min(d, lineDistance(line, lon, lat))
	}
	for i := 0; i+1 < len(g.points); i += 2 {
		d = math.Min(d, math.Hypot(g.points[i]-lon, g.points[i+1]-lat))
	}
	if d > bound {
		return math.Inf(1)
	}
	return d
}

// polygonContains is even-odd ray casting over every ring, so holes cancel the outer ring.
func polygonContains(rings [][]float64, x, y float64) bool {
	inside := false
	for _, r := range rings {
		if ringCrosses(r, x, y) {
			inside = !inside
		}
	}
	return inside
}

// ringCrosses reports whether a ray from the point towards +x crosses the ring an odd number of times.
func ringCrosses(r []float64, x, y float64) bool {
	n := len(r) / 2
	odd := false
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		xi, yi := r[2*i], r[2*i+1]
		xj, yj := r[2*j], r[2*j+1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			odd = !odd
		}
	}
	return odd
}

// ringDistance closes the ring as containment does, so an unclosed ring
// measures the same shape both ways; a closed one adds a zero-length segment.
func ringDistance(r []float64, x, y float64) float64 {
	d := lineDistance(r, x, y)
	if n := len(r) / 2; n > 1 {
		d = math.Min(d, segmentDistance(r[2*n-2], r[2*n-1], r[0], r[1], x, y))
	}
	return d
}

func lineDistance(l []float64, x, y float64) float64 {
	n := len(l) / 2
	switch n {
	case 0:
		return math.Inf(1)
	case 1:
		return math.Hypot(l[0]-x, l[1]-y)
	}
	d := math.Inf(1)
	for i := 0; i+1 < n; i++ {
		d = math.Min(d, segmentDistance(l[2*i], l[2*i+1], l[2*i+2], l[2*i+3], x, y))
	}
	return d
}

func segmentDistance(ax, ay, bx, by, x, y float64) float64 {
	dx, dy := bx-ax, by-ay
	t := 0.0
	if l2 := dx*dx + dy*dy; l2 > 0 {
		t = math.Max(0, math.Min(1, ((x-ax)*dx+(y-ay)*dy)/l2))
	}
	return math.Hypot(ax+t*dx-x, ay+t*dy-y)
}

const earthRadius = 6371000 // metres

// Window bounds latitude and wrapped longitude offsets for a search of m
// metres at lat. It returns degrees, or 180 if the search reaches a pole.
// Inputs must be a valid latitude and non-negative m.
func Window(lat, m float64) float64 {
	phi := math.Abs(lat) * math.Pi / 180
	radius := m / earthRadius
	if radius >= math.Pi/2-phi {
		return 180
	}
	// Maximum longitude is slightly poleward of lat, where the circle touches
	// a meridian. This offset also bounds latitude.
	lon := math.Asin(math.Min(1, math.Sin(radius)/math.Cos(phi))) * 180 / math.Pi
	// Round outward so the prefilter cannot drop a label Haversine accepts.
	return math.Min(180, lon+1e-9)
}

// Haversine is the great-circle distance in metres.
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dLon/2)*math.Sin(dLon/2)
	// Rounding at antipodes can put a slightly above 1.
	a = math.Min(1, a)
	return earthRadius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// WKB geometry types and EWKB flags.
const (
	wkbPoint              = 1
	wkbLineString         = 2
	wkbPolygon            = 3
	wkbMultiPoint         = 4
	wkbMultiLineString    = 5
	wkbMultiPolygon       = 6
	wkbGeometryCollection = 7
	ewkbZ                 = 0x80000000
	ewkbM                 = 0x40000000
	ewkbSRID              = 0x20000000
)

var errShort = errors.New("wkb: truncated")

type decoder struct {
	b   []byte
	pos int
	g   *Geometry
}

// Decode parses WKB or EWKB, either byte order, 2D coordinates kept, Z and M dropped.
func Decode(wkb []byte) (*Geometry, error) {
	d := &decoder{b: wkb, g: &Geometry{}}
	if err := d.geometry(); err != nil {
		return nil, err
	}
	if d.pos != len(wkb) {
		return nil, fmt.Errorf("wkb: %d trailing bytes", len(wkb)-d.pos)
	}
	d.g.finish()
	return d.g, nil
}

func (d *decoder) u8() (byte, error) {
	if d.pos+1 > len(d.b) {
		return 0, errShort
	}
	v := d.b[d.pos]
	d.pos++
	return v, nil
}

func (d *decoder) u32(bo binary.ByteOrder) (uint32, error) {
	if d.pos+4 > len(d.b) {
		return 0, errShort
	}
	v := bo.Uint32(d.b[d.pos:])
	d.pos += 4
	return v, nil
}

// coords reads n points of dims doubles each and returns their x,y pairs.
func (d *decoder) coords(bo binary.ByteOrder, n uint32, dims int) ([]float64, error) {
	need := int(n) * dims * 8
	if need < 0 || d.pos+need > len(d.b) {
		return nil, errShort
	}
	out := make([]float64, 0, 2*n)
	for i := uint32(0); i < n; i++ {
		out = append(out,
			math.Float64frombits(bo.Uint64(d.b[d.pos:])),
			math.Float64frombits(bo.Uint64(d.b[d.pos+8:])))
		d.pos += dims * 8
	}
	return out, nil
}

func (d *decoder) geometry() error {
	order, err := d.u8()
	if err != nil {
		return err
	}
	var bo binary.ByteOrder
	switch order {
	case 0:
		bo = binary.BigEndian
	case 1:
		bo = binary.LittleEndian
	default:
		return fmt.Errorf("wkb: byte order %d", order)
	}
	t, err := d.u32(bo)
	if err != nil {
		return err
	}
	dims := 2
	if t&ewkbZ != 0 {
		dims++
	}
	if t&ewkbM != 0 {
		dims++
	}
	srid := t&ewkbSRID != 0
	t &^= ewkbZ | ewkbM | ewkbSRID
	switch {
	case t >= 3000:
		dims, t = 4, t-3000
	case t >= 2000:
		dims, t = 3, t-2000
	case t >= 1000:
		dims, t = 3, t-1000
	}
	if srid {
		if _, err := d.u32(bo); err != nil {
			return err
		}
	}
	switch t {
	case wkbPoint:
		c, err := d.coords(bo, 1, dims)
		if err != nil {
			return err
		}
		if !math.IsNaN(c[0]) {
			d.g.points = append(d.g.points, c...)
		}
	case wkbLineString:
		n, err := d.u32(bo)
		if err != nil {
			return err
		}
		c, err := d.coords(bo, n, dims)
		if err != nil {
			return err
		}
		d.g.lines = append(d.g.lines, c)
	case wkbPolygon:
		nr, err := d.u32(bo)
		if err != nil {
			return err
		}
		var rings [][]float64
		for i := uint32(0); i < nr; i++ {
			n, err := d.u32(bo)
			if err != nil {
				return err
			}
			c, err := d.coords(bo, n, dims)
			if err != nil {
				return err
			}
			rings = append(rings, c)
		}
		if len(rings) > 0 {
			d.g.polys = append(d.g.polys, rings)
		}
	case wkbMultiPoint, wkbMultiLineString, wkbMultiPolygon, wkbGeometryCollection:
		n, err := d.u32(bo)
		if err != nil {
			return err
		}
		for i := uint32(0); i < n; i++ {
			if err := d.geometry(); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("wkb: geometry type %d", t)
	}
	return nil
}

// finish records a box for each polygon and the geometry's own box.
func (g *Geometry) finish() {
	none := Bbox{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	b := none
	g.boxes = make([]Bbox, len(g.polys))
	for i, poly := range g.polys {
		pb := none
		for _, r := range poly {
			pb = grow(pb, r)
		}
		g.boxes[i] = pb
		b = b.Union(pb)
	}
	for _, l := range g.lines {
		b = grow(b, l)
	}
	b = grow(b, g.points)
	g.empty = math.IsInf(b.XMin, 1)
	if !g.empty {
		g.bbox = b
	}
}

// grow extends the box to cover the x,y pairs in c.
func grow(b Bbox, c []float64) Bbox {
	for i := 0; i+1 < len(c); i += 2 {
		b.XMin, b.XMax = math.Min(b.XMin, c[i]), math.Max(b.XMax, c[i])
		b.YMin, b.YMax = math.Min(b.YMin, c[i+1]), math.Max(b.YMax, c[i+1])
	}
	return b
}
