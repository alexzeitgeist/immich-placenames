package cache

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexzeitgeist/immich-placenames/internal/geo"
)

// An index written before Primary and Common existed decodes with them
// empty, and the file reports neither local names nor translations.
func TestIndexWithoutPrimary(t *testing.T) {
	type oldRow struct {
		ID, Country, Name, Subtype, Class string
		AdminLevel                        int32
		Territorial, Land                 bool
		Bbox                              geo.Bbox
		Off                               int64
		Len                               int32
	}
	blob := wkbPoint(2, 3)
	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.Write(blob)
	off := int64(buf.Len())
	old := struct {
		Header Header
		Rows   []oldRow
	}{Header{Kind: "divisions", Code: "HR"}, []oldRow{{ID: "a", Country: "HR", Name: "Alpha", Subtype: "locality", AdminLevel: -1,
		Bbox: geo.Bbox{XMin: 1, YMin: 2, XMax: 3, YMax: 4}, Off: int64(len(magic)), Len: int32(len(blob))}}}
	if err := gob.NewEncoder(&buf).Encode(old); err != nil {
		t.Fatal(err)
	}
	buf.Write(binary.LittleEndian.AppendUint64(nil, uint64(off)))
	dest := filepath.Join(t.TempDir(), "HR.geo")
	if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.HasPrimary || f.HasCommon || len(f.Rows) != 1 || f.Rows[0].Name != "Alpha" || f.Rows[0].Primary != "" || f.Rows[0].Common != nil {
		t.Errorf("HasPrimary %t HasCommon %t rows %+v", f.HasPrimary, f.HasCommon, f.Rows)
	}
	if g, err := f.Geometry(0); err != nil || !g.Contains(2, 3) {
		t.Errorf("geometry: %v", err)
	}
}
