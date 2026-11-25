package gwob

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func BenchmarkCube1(b *testing.B) {
	buf := []byte(cubeObj)
	options := &ObjParserOptions{}
	for i := 0; i < b.N; i++ {
		NewObjFromBuf("cubeObj", buf, options)
	}
}

func BenchmarkRelativeIndex1(b *testing.B) {
	buf := []byte(relativeObj)
	options := &ObjParserOptions{}
	for i := 0; i < b.N; i++ {
		NewObjFromBuf("relativeObj", buf, options)
	}
}

func BenchmarkForwardVertex1(b *testing.B) {
	buf := []byte(forwardObj)
	options := &ObjParserOptions{}
	for i := 0; i < b.N; i++ {
		NewObjFromBuf("forwardObj", buf, options)
	}
}

const LogStats = false

func expectInt(t *testing.T, label string, want, got int) {
	if want != got {
		t.Errorf("%s: want=%d got=%d", label, want, got)
	}
}

func sliceEqualInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}

	for i, v := range a {
		if v != b[i] {
			return false
		}
	}

	return true
}

func sliceEqualFloat(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}

	for i, v := range a {
		if v != b[i] {
			return false
		}
	}

	return true
}

func TestCube(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestCube NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("cubeObj", []byte(cubeObj), &options)
	if err != nil {
		t.Errorf("TestCube: NewObjFromBuf: %v", err)
		return
	}

	if !sliceEqualInt(cubeIndices, o.Indices) {
		t.Errorf("TestCube: indices: want=%v got=%v", cubeIndices, o.Indices)
	}

	if !sliceEqualFloat(cubeCoord, o.Coord) {
		t.Errorf("TestCube: coord: want=%d%v got=%d%v", len(cubeCoord), cubeCoord, len(o.Coord), o.Coord)
	}

	if o.StrideSize != cubeStrideSize {
		t.Errorf("TestCube: stride size: want=%d got=%d", cubeStrideSize, o.StrideSize)
	}

	if o.StrideOffsetPosition != cubeStrideOffsetPosition {
		t.Errorf("TestCube: stride offset position: want=%d got=%d", cubeStrideOffsetPosition, o.StrideOffsetPosition)
	}

	if o.StrideOffsetTexture != cubeStrideOffsetTexture {
		t.Errorf("TestCube: stride offset texture: want=%d got=%d", cubeStrideOffsetTexture, o.StrideOffsetTexture)
	}

	if o.StrideOffsetNormal != cubeStrideOffsetNormal {
		t.Errorf("TestCube: stride offset normal: want=%d got=%d", cubeStrideOffsetNormal, o.StrideOffsetNormal)
	}
}

func TestWriteEmpty(t *testing.T) {

	// load
	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestWriteEmpty NewObjFromBuf: log: %s\n", msg) }}
	orig, err := NewObjFromBuf("empty", []byte{}, &options)
	if err != nil {
		t.Errorf("TestWriteEmpty: NewObjFromBuf: %v", err)
		return
	}

	// export
	buf := bytes.Buffer{}
	errWrite := orig.ToWriter(&buf)
	if errWrite != nil {
		t.Errorf("TestWriteEmpty: ToWriter: %v", errWrite)
		return
	}

	// reload
	_, errParse := NewObjFromReader("empty-reload", &buf, &options)
	if errParse != nil {
		t.Errorf("TestWriteEmpty: NewObjFromReader: %v", errParse)
		return
	}
}

func TestWriteBad(t *testing.T) {

	// load
	orig, err := NewObjFromVertex([]float32{}, []int{0})
	if err != nil {
		t.Errorf("TestWriteBad: NewObjFromVertex: %v", err)
		return
	}

	// export
	buf := bytes.Buffer{}
	errWrite := orig.ToWriter(&buf)
	if errWrite == nil {
		t.Errorf("TestWriteBad: unexpected writer success for bad group index count (non multiple of 3)")
		return
	}

}

func TestCubeWrite(t *testing.T) {

	// load cube
	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestCube NewObjFromBuf: log: %s\n", msg) }}
	orig, err := NewObjFromBuf("cube-orig", []byte(cubeObj), &options)
	if err != nil {
		t.Errorf("TestCubeWrite: NewObjFromBuf: %v", err)
		return
	}

	// export cube
	buf := bytes.Buffer{}
	errWrite := orig.ToWriter(&buf)
	if errWrite != nil {
		t.Errorf("TestCubeWrite: ToWriter: %v", errWrite)
		return
	}

	// reload cube
	o, errParse := NewObjFromReader("cube-reload", &buf, &options)
	if errParse != nil {
		t.Errorf("TestCubeWrite: NewObjFromReader: %v", errParse)
		return
	}

	if !sliceEqualInt(cubeIndices, o.Indices) {
		t.Errorf("TestCubeWrite: indices: want=%v got=%v", cubeIndices, o.Indices)
	}

	if !sliceEqualFloat(cubeCoord, o.Coord) {
		t.Errorf("TestCubeWrite: coord: want=%d%v got=%d%v", len(cubeCoord), cubeCoord, len(o.Coord), o.Coord)
	}

	if o.StrideSize != cubeStrideSize {
		t.Errorf("TestCubeWrite: stride size: want=%d got=%d", cubeStrideSize, o.StrideSize)
	}

	if o.StrideOffsetPosition != cubeStrideOffsetPosition {
		t.Errorf("TestCubeWrite: stride offset position: want=%d got=%d", cubeStrideOffsetPosition, o.StrideOffsetPosition)
	}

	if o.StrideOffsetTexture != cubeStrideOffsetTexture {
		t.Errorf("TestCubeWrite: stride offset texture: want=%d got=%d", cubeStrideOffsetTexture, o.StrideOffsetTexture)
	}

	if o.StrideOffsetNormal != cubeStrideOffsetNormal {
		t.Errorf("TestCubeWrite: stride offset normal: want=%d got=%d", cubeStrideOffsetNormal, o.StrideOffsetNormal)
	}
}

func TestRelativeIndex(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestRelativeIndex NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("relativeObj", []byte(relativeObj), &options)
	if err != nil {
		t.Errorf("TestRelativeIndex: NewObjFromBuf: %v", err)
		return
	}

	//indices := o.Indices[:len(o.Indices):len(o.Indices)]
	if !sliceEqualInt(relativeIndices, o.Indices) {
		t.Errorf("TestRelativeIndex: indices: want=%v got=%v", relativeIndices, o.Indices)
	}

	//coord := o.Coord[:len(o.Coord):len(o.Coord)]
	if !sliceEqualFloat(relativeCoord, o.Coord) {
		t.Errorf("TestRelativeIndex: coord: want=%v got=%v", relativeCoord, o.Coord)
	}
}

func TestForwardVertex(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestForwardVertex NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("forwardObj", []byte(forwardObj), &options)
	if err != nil {
		t.Errorf("TestForwardVertex: NewObjFromBuf: %v", err)
		return
	}

	if !sliceEqualInt(forwardIndices, o.Indices) {
		t.Errorf("TestForwardVertex: indices: want=%v got=%v", forwardIndices, o.Indices)
	}

	if !sliceEqualFloat(forwardCoord, o.Coord) {
		t.Errorf("TestForwardVertex: coord: want=%v got=%v", forwardCoord, o.Coord)
	}
}

func TestMisc(t *testing.T) {
	str := `
mtllib lib1

usemtl mtl1
usemtl mtl2

s off
s 1
`

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestMisc NewObjFromBuf: log: %s\n", msg) }}

	_, err := NewObjFromBuf("TestMisc local str obj", []byte(str), &options)
	if err != nil {
		t.Errorf("NewObjFromBuf: %v", err)
	}
}

func TestSkippedUV1(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestSkippedUV NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("skippedUV", []byte(skippedUVObj), &options)
	if err != nil {
		t.Errorf("TestSkippedUV: NewObjFromBuf: %v", err)
		return
	}

	if !sliceEqualInt(skippedUVIndices, o.Indices) {
		t.Errorf("TestSkippedUV: indices: want=%v got=%v", skippedUVIndices, o.Indices)
	}

	if !sliceEqualFloat(skippedUVCoord, o.Coord) {
		t.Errorf("TestSkippedUV: coord: want=%v got=%v", skippedUVCoord, o.Coord)
	}
}

func TestSkippedUV2(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestSkippedUV2 NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("skippedUV2", []byte(skippedUV2Obj), &options)
	if err != nil {
		t.Errorf("TestSkippedUV2: NewObjFromBuf: %v", err)
		return
	}

	if !sliceEqualInt(skippedUV2Indices, o.Indices) {
		t.Errorf("TestSkippedUV2: indices: want=%v got=%v", skippedUV2Indices, o.Indices)
	}

	if !sliceEqualFloat(skippedUV2Coord, o.Coord) {
		t.Errorf("TestSkippedUV2: coord: want=%v got=%v", skippedUV2Coord, o.Coord)
	}
}

func TestSmoothGroup1(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestSmoothGroup1 NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("smoothGroupObj1", []byte(smoothGroupObj1), &options)
	if err != nil {
		t.Errorf("TestSmoothGroup1: NewObjFromBuf: %v", err)
		return
	}

	if len(o.Groups) != 1 {
		t.Errorf("smoothGroupObj1: groups: want=%d got=%d", 1, len(o.Groups))
	}
}

func TestSmoothGroup2(t *testing.T) {

	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { fmt.Printf("TestSmoothGroup1 NewObjFromBuf: log: %s\n", msg) }}

	o, err := NewObjFromBuf("smoothGroupObj1", []byte(smoothGroupObj2), &options)
	if err != nil {
		t.Errorf("TestSmoothGroup2: NewObjFromBuf: %v", err)
		return
	}

	if len(o.Groups) != 1 {
		t.Errorf("smoothGroupObj2: groups: want=%d got=%d", 1, len(o.Groups))
	}
}

// badFaceObj mixes valid faces with bad faces (out-of-range vertex,
// texture and normal indices, malformed tokens) and material/smooth
// switches with no faces in between.
var badFaceObj = `
o bad_face_test
v 0 0 0
v 1 0 0
v 0 1 0
v 0 0 1
usemtl mtlA
f 1 2 3
f 1 2 99
f 1 2 nope
usemtl mtlB
s 1
usemtl mtlC
f 1 3 4
s 2
f 2 3 4
`

var badFaceIndices = []int{0, 1, 2, 0, 2, 3, 1, 2, 3}
var badFaceCoord = []float32{0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}

func TestBadFaceIsolation(t *testing.T) {

	var logs []string
	options := ObjParserOptions{LogStats: LogStats, Logger: func(msg string) { logs = append(logs, msg) }}

	o, err := NewObjFromBuf("badFaceObj", []byte(badFaceObj), &options)
	if err != nil {
		t.Fatalf("TestBadFaceIsolation: NewObjFromBuf: %v", err)
	}
	if o == nil {
		t.Fatalf("TestBadFaceIsolation: nil Obj")
	}

	// bad faces must not leak partial data into geometry
	if !sliceEqualInt(badFaceIndices, o.Indices) {
		t.Errorf("TestBadFaceIsolation: indices: want=%v got=%v", badFaceIndices, o.Indices)
	}
	if !sliceEqualFloat(badFaceCoord, o.Coord) {
		t.Errorf("TestBadFaceIsolation: coord: want=%v got=%v", badFaceCoord, o.Coord)
	}

	// skipped face count must be reported through the logger
	foundSkip := false
	for _, msg := range logs {
		if strings.Contains(msg, "skipped 2 bad face") {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Errorf("TestBadFaceIsolation: missing skipped-face log message: %v", logs)
	}

	// no residual zero-count groups; groups keep their own
	// smooth/usemtl; index ranges are contiguous
	if len(o.Groups) != 3 {
		t.Fatalf("TestBadFaceIsolation: groups: want=%d got=%d", 3, len(o.Groups))
	}
	wantBegin := []int{0, 3, 6}
	wantCount := []int{3, 3, 3}
	wantMtl := []string{"mtlA", "mtlC", "mtlC"}
	wantSmooth := []int{0, 1, 2}
	for i, g := range o.Groups {
		if g.IndexBegin != wantBegin[i] || g.IndexCount != wantCount[i] {
			t.Errorf("group %d: range want=%d+%d got=%d+%d", i, wantBegin[i], wantCount[i], g.IndexBegin, g.IndexCount)
		}
		if g.Usemtl != wantMtl[i] {
			t.Errorf("group %d: usemtl want=%s got=%s", i, wantMtl[i], g.Usemtl)
		}
		if g.Smooth != wantSmooth[i] {
			t.Errorf("group %d: smooth want=%d got=%d", i, wantSmooth[i], g.Smooth)
		}
	}
	for i := 1; i < len(o.Groups); i++ {
		prev := o.Groups[i-1]
		if prev.IndexBegin+prev.IndexCount != o.Groups[i].IndexBegin {
			t.Errorf("groups %d and %d not contiguous", i-1, i)
		}
	}

	// export must not fail and must round-trip
	buf := bytes.Buffer{}
	if errWrite := o.ToWriter(&buf); errWrite != nil {
		t.Fatalf("TestBadFaceIsolation: ToWriter: %v", errWrite)
	}
	re, errParse := NewObjFromReader("badFaceObj-reload", &buf, &options)
	if errParse != nil {
		t.Fatalf("TestBadFaceIsolation: reload: %v", errParse)
	}
	if !sliceEqualInt(o.Indices, re.Indices) {
		t.Errorf("TestBadFaceIsolation: reload indices: want=%v got=%v", o.Indices, re.Indices)
	}
	if !sliceEqualFloat(o.Coord, re.Coord) {
		t.Errorf("TestBadFaceIsolation: reload coord: want=%v got=%v", o.Coord, re.Coord)
	}
}

func TestBadFaceKinds(t *testing.T) {

	cases := map[string]string{
		"vertex out of range":  "v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 4\n",
		"vertex negative oob":  "v 0 0 0\nv 1 0 0\nv 0 1 0\nf -4 -2 -1\n",
		"vertex zero":          "v 0 0 0\nv 1 0 0\nv 0 1 0\nf 0 1 2\n",
		"texture out of range": "v 0 0 0\nv 1 0 0\nv 0 1 0\nvt 0 0\nf 1/1 2/1 3/2\n",
		"normal out of range":  "v 0 0 0\nv 1 0 0\nv 0 1 0\nvn 0 0 1\nf 1//1 2//2 3//1\n",
		"malformed index":      "v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 x\n",
		"too few corners":      "v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2\n",
		"quad with bad corner": "v 0 0 0\nv 1 0 0\nv 0 1 0\nv 0 0 1\nf 1 2 3 9\n",
	}

	for name, str := range cases {
		options := ObjParserOptions{}
		o, err := NewObjFromBuf(name, []byte(str), &options)
		if err != nil {
			t.Errorf("%s: NewObjFromBuf: %v", name, err)
			continue
		}
		if o == nil {
			t.Errorf("%s: nil Obj", name)
			continue
		}
		if len(o.Indices) != 0 || len(o.Coord) != 0 {
			t.Errorf("%s: bad face leaked data: indices=%v coord=%v", name, o.Indices, o.Coord)
		}
		for _, g := range o.Groups {
			if g.IndexCount == 0 {
				t.Errorf("%s: residual zero-count group=%s", name, g.Name)
			}
		}
	}
}

func TestAllFacesBad(t *testing.T) {

	str := `
o all_bad
v 0 0 0
v 1 0 0
usemtl m
f 1 2 3
f 4 5 6
s 1
f 7 8 9
`
	options := ObjParserOptions{}

	o, err := NewObjFromBuf("allBad", []byte(str), &options)
	if err != nil {
		t.Fatalf("TestAllFacesBad: NewObjFromBuf: %v", err)
	}
	if o == nil {
		t.Fatalf("TestAllFacesBad: nil Obj")
	}

	// must export safely as empty geometry
	buf := bytes.Buffer{}
	if errWrite := o.ToWriter(&buf); errWrite != nil {
		t.Fatalf("TestAllFacesBad: ToWriter: %v", errWrite)
	}
	if strings.Contains(buf.String(), "\nf ") {
		t.Errorf("TestAllFacesBad: unexpected face line in output: %q", buf.String())
	}

	// exported empty geometry must parse back
	re, errParse := NewObjFromReader("allBad-reload", &buf, &options)
	if errParse != nil {
		t.Fatalf("TestAllFacesBad: reload: %v", errParse)
	}
	if len(re.Indices) != 0 || len(re.Coord) != 0 {
		t.Errorf("TestAllFacesBad: reload not empty: indices=%v coord=%v", re.Indices, re.Coord)
	}
}

func TestNoResidualEmptyGroups(t *testing.T) {

	str := `
mtllib lib
usemtl a
usemtl b
s off
s 1
g g1
g g2
v 0 0 0
v 1 0 0
v 0 1 0
f 1 2 3
usemtl c
s 2
usemtl d
`
	options := ObjParserOptions{}

	o, err := NewObjFromBuf("residual", []byte(str), &options)
	if err != nil {
		t.Fatalf("TestNoResidualEmptyGroups: NewObjFromBuf: %v", err)
	}

	for _, g := range o.Groups {
		if g.IndexCount == 0 {
			t.Errorf("TestNoResidualEmptyGroups: residual zero-count group=%s", g.Name)
		}
	}
	if len(o.Groups) != 1 {
		t.Fatalf("TestNoResidualEmptyGroups: groups: want=1 got=%d", len(o.Groups))
	}
	g := o.Groups[0]
	if g.Name != "g2" || g.Usemtl != "b" || g.Smooth != 1 {
		t.Errorf("TestNoResidualEmptyGroups: group want=g2/b/1 got=%s/%s/%d", g.Name, g.Usemtl, g.Smooth)
	}
	if g.IndexBegin != 0 || g.IndexCount != 3 {
		t.Errorf("TestNoResidualEmptyGroups: range want=0+3 got=%d+%d", g.IndexBegin, g.IndexCount)
	}
}

func roundTrip(t *testing.T, label, str string) *Obj {
	options := ObjParserOptions{}

	orig, err := NewObjFromBuf(label, []byte(str), &options)
	if err != nil {
		t.Fatalf("%s: NewObjFromBuf: %v", label, err)
	}

	buf := bytes.Buffer{}
	if errWrite := orig.ToWriter(&buf); errWrite != nil {
		t.Fatalf("%s: ToWriter: %v", label, errWrite)
	}

	re, errParse := NewObjFromReader(label+"-reload", &buf, &options)
	if errParse != nil {
		t.Fatalf("%s: reload: %v", label, errParse)
	}

	if !sliceEqualInt(orig.Indices, re.Indices) {
		t.Errorf("%s: reload indices: want=%v got=%v", label, orig.Indices, re.Indices)
	}
	if !sliceEqualFloat(orig.Coord, re.Coord) {
		t.Errorf("%s: reload coord: want=%v got=%v", label, orig.Coord, re.Coord)
	}
	if orig.StrideSize != re.StrideSize ||
		orig.StrideOffsetPosition != re.StrideOffsetPosition ||
		orig.StrideOffsetTexture != re.StrideOffsetTexture ||
		orig.StrideOffsetNormal != re.StrideOffsetNormal {
		t.Errorf("%s: stride mismatch: want=%d/%d/%d/%d got=%d/%d/%d/%d", label,
			orig.StrideSize, orig.StrideOffsetPosition, orig.StrideOffsetTexture, orig.StrideOffsetNormal,
			re.StrideSize, re.StrideOffsetPosition, re.StrideOffsetTexture, re.StrideOffsetNormal)
	}

	return re
}

func TestWriteTextureOnly(t *testing.T) {
	str := `
v 0 0 0
v 1 0 0
v 0 1 0
vt 0 0
vt 1 0
vt 0 1
f 1/1 2/2 3/3
`
	roundTrip(t, "TestWriteTextureOnly", str)
}

func TestWriteNormalOnly(t *testing.T) {
	str := `
v 0 0 0
v 1 0 0
v 0 1 0
vn 1 0 0
vn 0 1 0
vn 0 0 1
f 1//1 2//2 3//3
`
	roundTrip(t, "TestWriteNormalOnly", str)
}

func TestWriteTextureAndNormal(t *testing.T) {
	roundTrip(t, "TestWriteTextureAndNormal", cubeObj)
}

func TestWriteBadCountNoFaceLines(t *testing.T) {

	orig, err := NewObjFromVertex([]float32{}, []int{0, 1})
	if err != nil {
		t.Fatalf("TestWriteBadCountNoFaceLines: NewObjFromVertex: %v", err)
	}

	buf := bytes.Buffer{}
	errWrite := orig.ToWriter(&buf)
	if errWrite == nil {
		t.Fatalf("TestWriteBadCountNoFaceLines: unexpected writer success")
	}
	if strings.Contains(buf.String(), "\nf ") {
		t.Errorf("TestWriteBadCountNoFaceLines: face line written for bad group: %q", buf.String())
	}
}

func TestConcurrentParse(t *testing.T) {

	options := ObjParserOptions{}

	want, err := NewObjFromBuf("cubeObj", []byte(cubeObj), &options)
	if err != nil {
		t.Fatalf("TestConcurrentParse: NewObjFromBuf: %v", err)
	}

	buf := []byte(cubeObj)
	const goroutines = 8
	results := make([]*Obj, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			o, e := NewObjFromBuf("cubeObj", buf, &ObjParserOptions{})
			if e != nil {
				t.Errorf("TestConcurrentParse: goroutine %d: %v", slot, e)
				return
			}
			results[slot] = o
		}(i)
	}
	wg.Wait()

	for i, o := range results {
		if o == nil {
			t.Errorf("TestConcurrentParse: goroutine %d: nil Obj", i)
			continue
		}
		if !sliceEqualInt(want.Indices, o.Indices) {
			t.Errorf("TestConcurrentParse: goroutine %d: indices mismatch", i)
		}
		if !sliceEqualFloat(want.Coord, o.Coord) {
			t.Errorf("TestConcurrentParse: goroutine %d: coord mismatch", i)
		}
		if len(o.Groups) != len(want.Groups) {
			t.Errorf("TestConcurrentParse: goroutine %d: groups mismatch", i)
		}
	}
}

func TestParseRecovery(t *testing.T) {

	options := ObjParserOptions{}

	// first parse skips bad faces
	if _, err := NewObjFromBuf("badFaceObj", []byte(badFaceObj), &options); err != nil {
		t.Fatalf("TestParseRecovery: bad parse: %v", err)
	}

	// a following valid parse must match a fresh valid parse
	want, errWant := NewObjFromBuf("cubeObj", []byte(cubeObj), &options)
	if errWant != nil {
		t.Fatalf("TestParseRecovery: fresh parse: %v", errWant)
	}
	got, errGot := NewObjFromBuf("cubeObj", []byte(cubeObj), &options)
	if errGot != nil {
		t.Fatalf("TestParseRecovery: second parse: %v", errGot)
	}

	if !sliceEqualInt(want.Indices, got.Indices) {
		t.Errorf("TestParseRecovery: indices mismatch")
	}
	if !sliceEqualFloat(want.Coord, got.Coord) {
		t.Errorf("TestParseRecovery: coord mismatch")
	}
	if len(want.Groups) != len(got.Groups) {
		t.Errorf("TestParseRecovery: groups mismatch")
	}
}

var cubeStrideSize = 32
var cubeStrideOffsetPosition = 0
var cubeStrideOffsetTexture = 12
var cubeStrideOffsetNormal = 20
var cubeIndices = []int{0, 1, 2, 2, 3, 0, 4, 5, 6, 6, 7, 4, 8, 9, 10, 10, 11, 8, 12, 13, 14, 14, 15, 12, 16, 17, 18, 18, 19, 16, 20, 21, 22, 22, 23, 20}
var cubeCoord = []float32{1, -1, 1, 0.5, 0, 0, -1, 0, -1, -1, 1, 0.5, 0, 0, -1, 0, -1, -1, -1, 0.5, 0, 0, -1, 0, 1, -1, -1, 0.5, 0, 0, -1, 0, 1, 1, -1, 0.5, 0, 0, 1, 0, -1, 1, -1, 0.5, 0, 0, 1, 0, -1, 1, 1, 0.5, 0, 0, 1, 0, 1, 1, 1, 0.5, 0, 0, 1, 0, 1, -1, -1, 0, 0, 1, 0, 0, 1, 1, -1, 0, 0, 1, 0, 0, 1, 1, 1, 0, 0, 1, 0, 0, 1, -1, 1, 0, 0, 1, 0, 0, -1, -1, 1, 0, 0, -1, 0, 0, -1, 1, 1, 0, 0, -1, 0, 0, -1, 1, -1, 0, 0, -1, 0, 0, -1, -1, -1, 0, 0, -1, 0, 0, 1, -1, 1, 1, 0, 0, 0, 1, 1, 1, 1, 1, 0, 0, 0, 1, -1, 1, 1, 1, 0, 0, 0, 1, -1, -1, 1, 1, 0, 0, 0, 1, -1, -1, -1, 1, 0, 0, 0, -1, -1, 1, -1, 1, 0, 0, 0, -1, 1, 1, -1, 1, 0, 0, 0, -1, 1, -1, -1, 1, 0, 0, 0, -1}

var relativeIndices = []int{0, 1, 2, 0, 1, 2, 3, 4, 5, 3, 4, 5, 0, 1, 2, 0, 1, 2}
var relativeCoord = []float32{1.0, 1.0, 1.0, 2.0, 2.0, 2.0, 3.0, 3.0, 3.0, 4.0, 4.0, 4.0, 5.0, 5.0, 5.0, 6.0, 6.0, 6.0}

var forwardIndices = []int{0, 1, 2}
var forwardCoord = []float32{1.0, 1.0, 1.0, 2.0, 2.0, 2.0, 3.0, 3.0, 3.0}

var cubeObj = `
# texture_cube.obj

mtllib texture_cube.mtl

o cube

# square bottom
v -1 -1 -1
v -1 -1 1
v 1 -1 1
v 1 -1 -1

# square top
v -1 1 -1
v -1 1 1
v 1 1 1
v 1 1 -1

# uv coord

# red -3
vt 0 0

# green -2
vt .5 0

# blue -1
vt 1 0

# normal coord

# down -6
vn 0 -1 0

# up -5
vn 0 1 0

# right -4
vn 1 0 0

# left -3
vn -1 0 0

# front -2
vn 0 0 1

# back -1
vn 0 0 -1

usemtl 3-pixel-rgb

# face down (green -2)
f -6/-2/-6 -7/-2/-6 -8/-2/-6
f -8/-2/-6 -5/-2/-6 -6/-2/-6

# face up (green -2)
f -1/-2/-5 -4/-2/-5 -3/-2/-5
f -3/-2/-5 -2/-2/-5 -1/-2/-5 

# face right (red -3)
f -5/-3/-4 -1/-3/-4 -2/-3/-4
f -2/-3/-4 -6/-3/-4 -5/-3/-4

# face left (red -3)
f -7/-3/-3 -3/-3/-3 -4/-3/-3
f -4/-3/-3 -8/-3/-3 -7/-3/-3

# face front (blue -1)
f -6/-1/-2 -2/-1/-2 -3/-1/-2
f -3/-1/-2 -7/-1/-2 -6/-1/-2

# face back (blue -1)
f -8/-1/-1 -4/-1/-1 -1/-1/-1
f -1/-1/-1 -5/-1/-1 -8/-1/-1
`

var relativeObj = `
o relative_test
v 1 1 1
v 2 2 2
v 3 3 3
f 1 2 3
# this line should affect indices, but not vertex array
f -3 -2 -1
v 4 4 4
v 5 5 5
v 6 6 6
f 4 5 6
# this line should affect indices, but not vertex array
f -3 -2 -1
# these lines should affect indices, but not vertex array
f 1 2 3
f -6 -5 -4
`

var forwardObj = `
o forward_vertices_test
# face pointing to forward vertex definitions
# support for this isn't usual in OBJ parsers
# since it requires multiple passes
# but currently we do support this layout
f 1 2 3
v 1 1 1
v 2 2 2
v 3 3 3
`

var skippedUVObj = `

o skipped_uv

v 1 1 1
v 2 2 2
v 3 3 3

vn 1 0 0
vn 0 1 0
vn 0 0 1

f 1//1 2//2 3//3 
`

var skippedUVIndices = []int{0, 1, 2}
var skippedUVCoord = []float32{1, 1, 1, 1, 0, 0, 2, 2, 2, 0, 1, 0, 3, 3, 3, 0, 0, 1}

var skippedUV2Obj = `

o skipped_uv

v 1 1 1
v 2 2 2
v 3 3 3

vt 0 0
vt .5 .5
vt 1 1

vn 1 0 0
vn 0 1 0
vn 0 0 1

f 1//1 2//2 3//3 
`

var skippedUV2Indices = []int{0, 1, 2}
var skippedUV2Coord = []float32{1, 1, 1, 1, 0, 0, 2, 2, 2, 0, 1, 0, 3, 3, 3, 0, 0, 1}

const smoothGroupObj1 = `
o Cube.001
v 1 -1 1
v -1 -1 1
v 1 1 1
v -1 1 1
v 1 -1 -1
v 1 1 -1
v -1 1 -1
v -1 -1 -1
vt 0 0
vt 1 0
vt 0 1
vt 1 1
vt 1 0
vt 1 1
vt 0 1
vt 0 0
vn 0 -1 0
vn 0 1 0
vn 1 0 0
vn -1 0 0
vn 0 0 -1
vn 0 0 1
f 1/1/6 3/3/6 4/4/6
f 1/1/6 4/4/6 2/2/6
f 1/1/1 2/2/1 8/8/1
f 1/1/1 8/8/1 5/5/1
f 1/1/3 5/5/3 6/6/3
f 1/1/3 6/6/3 3/3/3
`

const smoothGroupObj2 = `
o Cube.001
v 1 -1 1
v -1 -1 1
v 1 1 1
v -1 1 1
v 1 -1 -1
v 1 1 -1
v -1 1 -1
v -1 -1 -1
vt 0 0
vt 1 0
vt 0 1
vt 1 1
vt 1 0
vt 1 1
vt 0 1
vt 0 0
vn 0 -1 0
vn 0 1 0
vn 1 0 0
vn -1 0 0
vn 0 0 -1
vn 0 0 1
s 1
f 1/1/6 3/3/6 4/4/6
f 1/1/6 4/4/6 2/2/6
f 1/1/1 2/2/1 8/8/1
f 1/1/1 8/8/1 5/5/1
f 1/1/3 5/5/3 6/6/3
f 1/1/3 6/6/3 3/3/3
`
