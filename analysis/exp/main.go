package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"prismplate/internal/geom"
	"prismplate/internal/raster"
	"prismplate/internal/scene"
	"prismplate/internal/validate"
	"prismplate/internal/vector"
	"runtime"
	"sort"
	"crypto/sha256"
	"time"
)

func pngBytes(img *image.NRGBA) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func compileChecked(d scene.Document) scene.Compiled {
	c, err := validate.Compile(d)
	if err != nil {
		fmt.Printf("  COMPILE REJECT: %v\n", err)
		os.Exit(1)
	}
	return c
}

// coverage reports the fraction of sample points covered by at least one
// shape (production semantics), used to classify scene density.
func coverage(c scene.Compiled) float64 {
	hits := 0
	d := c.Document
	total := 4 * d.Width * d.Height
	for y := 0; y < d.Height; y++ {
		for x := 0; x < d.Width; x++ {
			for sy := 0; sy < 2; sy++ {
				for sx := 0; sx < 2; sx++ {
					p := geom.Point{X: float64(x) + (float64(sx)+0.5)/2, Y: float64(y) + (float64(sy)+0.5)/2}
					hit := false
					for _, l := range c.Layers {
						if len(l.Clip) > 0 && !l.Clip.Contains(p, "nonzero") {
							continue
						}
						if len(l.Mask) > 0 && !l.Mask.Contains(p, "nonzero") {
							continue
						}
						for _, s := range l.Shapes {
							if s.Path.Contains(s.Inverse.Apply(p), s.Shape.FillRule) {
								hit = true
								break
							}
						}
						if hit {
							break
						}
					}
					if hit {
						hits++
					}
				}
			}
		}
	}
	return float64(hits) / float64(total)
}

func cmdVerify(docs []scene.Document, names []string) {
	fmt.Println("== verify: PNG byte identity vs production raster.Render ==")
	fail := 0
	for i, d := range docs {
		c := compileChecked(d)
		ref, err := raster.Render(context.Background(), c)
		if err != nil {
			fmt.Printf("  %-22s production render error: %v\n", names[i], err)
			fail++
			continue
		}
		var ct0 Counters
		base := pngBytes(renderS0(c, &ct0))
		if !bytes.Equal(base, ref) {
			fmt.Printf("  %-22s S0-clone DIVERGES from production (harness bug)\n", names[i])
			fail++
			continue
		}
		type variant struct {
			name string
			img  *image.NRGBA
		}
		vars := []variant{}
		var ct Counters
		ct = Counters{}
		img1, _ := renderS1(c, &ct)
		vars = append(vars, variant{"S1-full-layer", img1})
		for _, t := range []int{32, 64, 128, 160} {
			ct = Counters{}
			vars = append(vars, variant{fmt.Sprintf("S2-tile%d", t), renderS2(c, t, &ct)})
			ct = Counters{}
			img3, _ := renderS3(c, t, &ct, false)
			vars = append(vars, variant{fmt.Sprintf("S3-tile%d", t), img3})
			ct = Counters{}
			img3h, _ := renderS3(c, t, &ct, true)
			vars = append(vars, variant{fmt.Sprintf("S3h-tile%d", t), img3h})
		}
		for _, v := range vars {
			got := pngBytes(v.img)
			if !bytes.Equal(got, ref) {
				diff := 0
				for j := 0; j < len(got) && j < len(ref); j++ {
					if got[j] != ref[j] {
						diff++
					}
				}
				fmt.Printf("  %-22s %-12s MISMATCH (%d bytes differ, len %d vs %d)\n", names[i], v.name, diff, len(got), len(ref))
				fail++
			}
		}
		fmt.Printf("  %-22s segments=%-5d coverage=%5.1f%%  S0==prod, %d variants identical\n",
			names[i], c.Segments, 100*coverage(c), len(vars))
	}
	if fail > 0 {
		fmt.Printf("VERIFY FAILED: %d problems\n", fail)
		os.Exit(1)
	}
	fmt.Println("VERIFY OK: all strategies bit-identical to production on all scenes")
}

type row struct {
	scene, strat string
	tile         int
	ms           float64
	ct           Counters
	bufBytes     int64
	allocBytes   uint64
}

func measureOne(name, strat string, tile int, f func(ct *Counters) (*image.NRGBA, int64)) row {
	var ct Counters
	var buf int64
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	_, buf = f(&ct)
	runtime.ReadMemStats(&m1)
	alloc := m1.TotalAlloc - m0.TotalAlloc
	best := time.Duration(1<<63 - 1)
	for r := 0; r < 5; r++ {
		var c2 Counters
		t0 := time.Now()
		_, b2 := f(&c2)
		dt := time.Since(t0)
		if dt < best {
			best = dt
		}
		buf = b2
		ct = c2
	}
	return row{scene: name, strat: strat, tile: tile, ms: float64(best) / 1e6, ct: ct, bufBytes: buf, allocBytes: alloc}
}

func cmdMeasure(docs []scene.Document, names []string) {
	fmt.Println("== measure: compute / allocation / boundary cost ==")
	var rows []row
	for i, d := range docs {
		c := compileChecked(d)
		nm := names[i]
		rows = append(rows, measureOne(nm, "S0-baseline", 0, func(ct *Counters) (*image.NRGBA, int64) {
			return renderS0(c, ct), 0
		}))
		rows = append(rows, measureOne(nm, "S1-full-layer", 0, func(ct *Counters) (*image.NRGBA, int64) {
			return renderS1(c, ct)
		}))
		for _, t := range []int{64, 128, 160} {
			t := t
			rows = append(rows, measureOne(nm, "S2-tiled", t, func(ct *Counters) (*image.NRGBA, int64) {
				return renderS2(c, t, ct), 0
			}))
			rows = append(rows, measureOne(nm, "S3-tiled-span", t, func(ct *Counters) (*image.NRGBA, int64) {
				return renderS3(c, t, ct, false)
			}))
		}
		rows = append(rows, measureOne(nm, "S3h-span+hoist", 128, func(ct *Counters) (*image.NRGBA, int64) {
			return renderS3(c, 128, ct, true)
		}))
	}
	fmt.Printf("%-22s %-13s %-4s %9s %12s %10s %10s %10s %10s %12s %12s\n",
		"scene", "strategy", "tile", "ms", "edgeIters", "crossX", "contains", "samples", "overs", "bufBytes", "allocBytes")
	for _, r := range rows {
		fmt.Printf("%-22s %-13s %-4d %9.2f %12d %10d %10d %10d %10d %12d %12d\n",
			r.scene, r.strat, r.tile, r.ms, r.ct.EdgeIters, r.ct.CrossX, r.ct.Contains, r.ct.Samples, r.ct.Overs, r.bufBytes, r.allocBytes)
	}
	fmt.Println("\n== tile sweep (boundary sensitivity) ==")
	for i, d := range docs {
		if names[i] != "boundary-512" && names[i] != "dense-stack-512" && names[i] != "sparse-512" {
			continue
		}
		c := compileChecked(d)
		for _, t := range []int{32, 64, 128, 160} {
			t := t
			r2 := measureOne(names[i], "S2-tiled", t, func(ct *Counters) (*image.NRGBA, int64) {
				return renderS2(c, t, ct), 0
			})
			r3 := measureOne(names[i], "S3-tiled-span", t, func(ct *Counters) (*image.NRGBA, int64) {
				return renderS3(c, t, ct, false)
			})
			fmt.Printf("%-16s tile=%-4d S2: %8.2fms edge=%10d | S3: %8.2fms edge=%10d buf=%9d\n",
				names[i], t, r2.ms, r2.ct.EdgeIters, r3.ms, r3.ct.EdgeIters, r3.bufBytes)
		}
	}
}

func cmdMemory() {
	fmt.Println("== memory: float64 intermediate buffer budget 8MiB per worker ==")
	const budget = 8 << 20
	fmt.Printf("budget = %d bytes (8 MiB); float64 RGBA pixel = 32 bytes; sample grid = 2W x 2H\n\n", budget)
	fmt.Println("full-layer offscreen (S1): dst+src = 2 buffers x (2W)(2H) x 32B = 256*W*H bytes")
	fmt.Printf("%-14s %10s %14s  %s\n", "canvas", "W*H", "S1 bytes", "verdict")
	for _, wh := range [][2]int{{64, 64}, {128, 128}, {192, 144}, {128, 256}, {256, 256}, {512, 512}} {
		w, h := wh[0], wh[1]
		b := 256 * w * h
		v := "OK"
		if b > budget {
			v = "EXCEEDS 8MiB"
		}
		fmt.Printf("%-14s %10d %14d  %s\n", fmt.Sprintf("%dx%d", w, h), w*h, b, v)
	}
	fmt.Println("\ntiled offscreen (S3): dst+src = 2 buffers x (2t)^2 x 32B = 256*t^2 bytes (canvas-independent)")
	fmt.Printf("%-6s %14s  %s\n", "tile", "S3 bytes", "verdict")
	for _, t := range []int{32, 64, 128, 160, 181, 256} {
		b := 256 * t * t
		v := "OK"
		if b > budget {
			v = "EXCEEDS 8MiB"
		}
		fmt.Printf("%-6d %14d  %s\n", t, b, v)
	}
}

// cmdCounter shows numerically that splitting a layer's buffer above the
// group-opacity/blend application point changes the result, i.e. per-shape
// opacity/blend is not an equivalent substitute for group semantics.
func cmdCounter() {
	fmt.Println("== counterexamples: group boundary violations are not equivalent ==")
	bg := scene.Color{R: 0.2, G: 0.2, B: 0.2, A: 1}
	s1 := scene.Color{R: 0.8, G: 0.2, B: 0.2, A: 0.6}
	s2 := scene.Color{R: 0.1, G: 0.5, B: 0.9, A: 0.6}
	dst0 := pixelFrom(bg)
	p1, p2 := pixelFrom(s1), pixelFrom(s2)
	over := func(d, s fpx, mode string) fpx {
		blend := func(dd, ss float64) float64 {
			if mode == "normal" || s[3] == 0 || d[3] == 0 {
				return ss + dd*(1-s[3])
			}
			a, b := dd/d[3], ss/s[3]
			v := a * b
			if mode == "screen" {
				v = a + b - a*b
			}
			return (1-s[3])*dd + (1-d[3])*ss + s[3]*d[3]*v
		}
		return fpx{blend(d[0], s[0]), blend(d[1], s[1]), blend(d[2], s[2]), s[3] + d[3]*(1-s[3])}
	}
	scale := func(p fpx, a float64) fpx { return fpx{p[0] * a, p[1] * a, p[2] * a, p[3] * a} }
	for _, mode := range []string{"normal", "multiply", "screen"} {
		alpha := 0.5
		// group semantics (production PNG + SVG <g opacity mix-blend-mode>):
		// aggregate the layer first, then apply opacity and blend once.
		agg := over(fpx{}, p1, "normal")
		agg = over(agg, p2, "normal")
		group := over(dst0, scale(agg, alpha), mode)
		// wrong split A: opacity applied per shape
		splitA := over(over(dst0, scale(p1, alpha), mode), scale(p2, alpha), mode)
		// wrong split B: blend mode applied per shape, no group opacity
		splitB := over(over(dst0, p1, mode), p2, mode)
		fmt.Printf("mode=%-8s alpha=%.1f overlap region:\n", mode, alpha)
		fmt.Printf("  group (correct)        RGBA8 = %v\n", rgba8(group))
		fmt.Printf("  per-shape opacity      RGBA8 = %v  <- PNG would diverge from SVG <g opacity>\n", rgba8(splitA))
		fmt.Printf("  per-shape blend, no op RGBA8 = %v\n", rgba8(splitB))
	}
	fmt.Println("\nconclusion: buffer splits are only safe BELOW the single Scale+blend")
	fmt.Println("point of a layer (partial srcs combined with plain normal Over, in order).")
}

type fpx [4]float64

func pixelFrom(c scene.Color) fpx {
	lin := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return fpx{lin(c.R) * c.A, lin(c.G) * c.A, lin(c.B) * c.A, c.A}
}

func rgba8(p fpx) [4]uint8 {
	sr := func(v float64) float64 {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		if v <= 0.0031308 {
			return v * 12.92
		}
		return 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	if p[3] <= 0 {
		return [4]uint8{}
	}
	cl := func(v float64) float64 {
		if v > 1 {
			return 1
		}
		return v
	}
	return [4]uint8{uint8(sr(p[0]/p[3])*255 + 0.5), uint8(sr(p[1]/p[3])*255 + 0.5), uint8(sr(p[2]/p[3])*255 + 0.5), uint8(cl(p[3])*255 + 0.5)}
}

func cmdE2E(amberPath, httpPNG string) {
	fmt.Println("== e2e: HTTP-service PNG vs in-process strategies ==")
	d, err := loadAmber(amberPath)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	c := compileChecked(d)
	served, err := os.ReadFile(httpPNG)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	local, err := raster.Render(context.Background(), c)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("  http bytes=%d sha=%x\n", len(served), sha256.Sum256(served))
	fmt.Printf("  prod==http: %v\n", bytes.Equal(local, served))
	var ct Counters
	s3, _ := renderS3(c, 128, &ct, true)
	fmt.Printf("  S3h==http:  %v\n", bytes.Equal(pngBytes(s3), served))
	svg := vector.Export(c)
	fmt.Printf("  svg bytes=%d (vector proof, group semantics via <g opacity>+mix-blend-mode)\n", len(svg))
}

func main() {
	mode := flag.String("mode", "all", "verify|measure|memory|counter|e2e|all")
	amber := flag.String("amber", "examples/amber-proof.json", "path to amber example")
	httpPNG := flag.String("http-png", "", "path to a PNG fetched from the live service (e2e)")
	flag.Parse()
	if *mode == "memory" {
		cmdMemory()
		return
	}
	if *mode == "counter" {
		cmdCounter()
		return
	}
	if *mode == "e2e" {
		cmdE2E(*amber, *httpPNG)
		return
	}
	docs, names, err := sceneSet(*amber)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	idx := make([]int, len(docs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return names[idx[a]] < names[idx[b]] })
	var sd []scene.Document
	var sn []string
	for _, i := range idx {
		sd = append(sd, docs[i])
		sn = append(sn, names[i])
	}
	switch *mode {
	case "verify":
		cmdVerify(sd, sn)
	case "measure":
		cmdMeasure(sd, sn)
	case "all":
		cmdVerify(sd, sn)
		fmt.Println()
		cmdMeasure(sd, sn)
		fmt.Println()
		cmdMemory()
		fmt.Println()
		cmdCounter()
	default:
		fmt.Println("unknown mode")
		os.Exit(1)
	}
}
