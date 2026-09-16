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

// distanceSq is the squared planar distance in degrees from the point to the
// box, zero inside.
func (b Bbox) distanceSq(lon, lat float64) float64 {
	dx, dy := 0.0, 0.0
	if lon < b.XMin {
		dx = b.XMin - lon
	} else if lon > b.XMax {
		dx = lon - b.XMax
	}
	if lat < b.YMin {
		dy = b.YMin - lat
	} else if lat > b.YMax {
		dy = lat - b.YMax
	}
	return dx*dx + dy*dy
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

// chunkSize is the number of consecutive segments per box.
const chunkSize = 64

// ring stores x0,y0,x1,y1,... with one box per chunk of segments.
type ring struct {
	pts   []float64
	boxes []Bbox
}

// Geometry is a decoded WKB geometry: polygons with holes, lines and points.
type Geometry struct {
	polys  [][]ring // polygon → ring; the first ring is the outer one
	boxes  []Bbox   // one box per polygon
	lines  []ring
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

// distanceWithin returns +Inf beyond bound. Polygon and chunk boxes give
// lower bounds on edge distance. Round the squared limit up so pruning cannot
// reject a distance that passes the final comparison after taking its root.
func (g *Geometry) distanceWithin(lon, lat, bound float64) float64 {
	d, limit := math.Inf(1), math.Nextafter(bound*bound, math.Inf(1))
	for i, poly := range g.polys {
		if g.boxes[i].distanceSq(lon, lat) > limit {
			continue
		}
		for _, r := range poly {
			if v := ringDistance(r, lon, lat, limit); v < d {
				d = v
				if v < limit {
					limit = v
				}
			}
		}
	}
	for _, l := range g.lines {
		if v := lineDistance(l, lon, lat, limit); v < d {
			d = v
			if v < limit {
				limit = v
			}
		}
	}
	for i := 0; i+1 < len(g.points); i += 2 {
		dx, dy := g.points[i]-lon, g.points[i+1]-lat
		if v := dx*dx + dy*dy; v < d {
			d = v
		}
	}
	if d = math.Sqrt(d); d > bound {
		return math.Inf(1)
	}
	return d
}

// polygonContains is even-odd ray casting over every ring, so holes cancel the outer ring.
func polygonContains(rings []ring, x, y float64) bool {
	inside := false
	for _, r := range rings {
		if ringCrosses(r, x, y) {
			inside = !inside
		}
	}
	return inside
}

// ringCrosses reports whether a ray towards +x crosses the ring an odd number of times.
func ringCrosses(r ring, x, y float64) bool {
	n := len(r.pts) / 2
	if n < 2 {
		return false
	}
	segs, odd := n-1, false
	for c, b := range r.boxes {
		if b.YMin > y || b.YMax <= y || b.XMax <= x {
			continue
		}
		for i, hi := c*chunkSize, min(c*chunkSize+chunkSize, segs); i < hi; i++ {
			if crosses(r.pts[2*i], r.pts[2*i+1], r.pts[2*i+2], r.pts[2*i+3], x, y) {
				odd = !odd
			}
		}
	}
	// Chunks exclude the closing segment from the last point to the first.
	if crosses(r.pts[2*n-2], r.pts[2*n-1], r.pts[0], r.pts[1], x, y) {
		odd = !odd
	}
	return odd
}

// crosses reports whether the segment passes the ray to the right of x.
func crosses(ax, ay, bx, by, x, y float64) bool {
	return (ay > y) != (by > y) && x < (bx-ax)*(y-ay)/(by-ay)+ax
}

// ringDistance closes the ring as containment does, so an unclosed ring
// measures the same shape both ways; a closed one adds a zero-length segment.
// Distances are squared, as in distanceWithin.
func ringDistance(r ring, x, y, limit float64) float64 {
	d := lineDistance(r, x, y, limit)
	if n := len(r.pts) / 2; n > 1 {
		if v := segmentDistance(r.pts[2*n-2], r.pts[2*n-1], r.pts[0], r.pts[1], x, y); v < d {
			d = v
		}
	}
	return d
}

// lineDistance returns a squared distance, skipping chunks beyond limit
// or the nearest segment found so far.
func lineDistance(l ring, x, y, limit float64) float64 {
	n := len(l.pts) / 2
	switch n {
	case 0:
		return math.Inf(1)
	case 1:
		dx, dy := l.pts[0]-x, l.pts[1]-y
		return dx*dx + dy*dy
	}
	segs, d := n-1, math.Inf(1)
	for c, b := range l.boxes {
		if b.distanceSq(x, y) > limit {
			continue
		}
		for i, hi := c*chunkSize, min(c*chunkSize+chunkSize, segs); i < hi; i++ {
			if v := segmentDistance(l.pts[2*i], l.pts[2*i+1], l.pts[2*i+2], l.pts[2*i+3], x, y); v < d {
				d = v
				if v < limit {
					limit = v
				}
			}
		}
	}
	return d
}

// segmentDistance is the squared planar distance from the point to the segment.
func segmentDistance(ax, ay, bx, by, x, y float64) float64 {
	dx, dy := bx-ax, by-ay
	px, py := ax, ay
	if l2 := dx*dx + dy*dy; l2 > 0 {
		t := ((x-ax)*dx + (y-ay)*dy) / l2
		switch {
		case t >= 1:
			// Use the stored endpoint: ax+(bx-ax) can round outside
			// the box and invalidate its distance lower bound.
			px, py = bx, by
		case t > 0:
			// Keep rounded interior projections inside the box too.
			px = max(min(ax, bx), min(max(ax, bx), ax+t*dx))
			py = max(min(ay, by), min(max(ay, by), ay+t*dy))
		}
	}
	px, py = px-x, py-y
	return px*px + py*py
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
		d.g.lines = append(d.g.lines, ring{pts: c})
	case wkbPolygon:
		nr, err := d.u32(bo)
		if err != nil {
			return err
		}
		var rings []ring
		for i := uint32(0); i < nr; i++ {
			n, err := d.u32(bo)
			if err != nil {
				return err
			}
			c, err := d.coords(bo, n, dims)
			if err != nil {
				return err
			}
			rings = append(rings, ring{pts: c})
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

// finish builds the chunk, polygon and geometry boxes.
func (g *Geometry) finish() {
	none := Bbox{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	b := none
	g.boxes = make([]Bbox, len(g.polys))
	for i, poly := range g.polys {
		pb := none
		for j := range poly {
			var rb Bbox
			poly[j].boxes, rb = chunks(poly[j].pts)
			pb = pb.Union(rb)
		}
		g.boxes[i] = pb
		b = b.Union(pb)
	}
	for i := range g.lines {
		var rb Bbox
		g.lines[i].boxes, rb = chunks(g.lines[i].pts)
		b = b.Union(rb)
	}
	b = grow(b, g.points)
	g.empty = math.IsInf(b.XMin, 1)
	if !g.empty {
		g.bbox = b
	}
}

// chunks returns one box per chunkSize segments and their union.
func chunks(pts []float64) ([]Bbox, Bbox) {
	none := Bbox{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	n := len(pts) / 2
	if n < 2 {
		return nil, grow(none, pts)
	}
	segs := n - 1
	out, all := make([]Bbox, (segs+chunkSize-1)/chunkSize), none
	for c := range out {
		// Segments [lo, hi) need points [lo, hi].
		lo, hi := c*chunkSize, min(c*chunkSize+chunkSize, segs)
		out[c] = grow(none, pts[2*lo:2*hi+2])
		all = all.Union(out[c])
	}
	return out, all
}

// grow extends the box to cover the x,y pairs in c.
func grow(b Bbox, c []float64) Bbox {
	for i := 0; i+1 < len(c); i += 2 {
		b.XMin, b.XMax = math.Min(b.XMin, c[i]), math.Max(b.XMax, c[i])
		b.YMin, b.YMax = math.Min(b.YMin, c[i+1]), math.Max(b.YMax, c[i+1])
	}
	return b
}
