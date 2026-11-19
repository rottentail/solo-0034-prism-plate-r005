package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"prismplate/internal/geom"
	"prismplate/internal/scene"
)

// lcg is a deterministic generator so scenes are reproducible across runs.
type lcg struct{ s uint64 }

func (l *lcg) next() float64 {
	l.s = l.s*6364136223846793005 + 1442695040888963407
	return float64(l.s>>11) / (1 << 53)
}

func quad(id string, x, y, w, h float64, brush scene.Brush, m geom.Matrix) scene.Shape {
	return scene.Shape{
		ID:       id,
		FillRule: "nonzero",
		Path: []geom.Op{
			{Kind: "M", Points: []geom.Point{{X: x, Y: y}}},
			{Kind: "L", Points: []geom.Point{{X: x + w, Y: y}}},
			{Kind: "L", Points: []geom.Point{{X: x + w, Y: y + h}}},
			{Kind: "L", Points: []geom.Point{{X: x, Y: y + h}}},
			{Kind: "Z"},
		},
		Brush:     brush,
		Transform: m,
	}
}

func solid(r, g, b, a float64) scene.Brush {
	return scene.Brush{Kind: "solid", Color: scene.Color{R: r, G: g, B: b, A: a}}
}

func gradient(x0, y0, x1, y1 float64, c0, c1 scene.Color) scene.Brush {
	return scene.Brush{
		Kind: "linear",
		From: geom.Point{X: x0, Y: y0},
		To:   geom.Point{X: x1, Y: y1},
		Stops: []scene.Stop{
			{At: 0, Color: c0},
			{At: 1, Color: c1},
		},
	}
}

// rotate returns an affine matrix rotating about (cx, cy).
func rotate(theta, cx, cy float64) geom.Matrix {
	sn, cs := math.Sin(theta), math.Cos(theta)
	return geom.Matrix{cs, sn, -sn, cs, cx - cs*cx + sn*cy, cy - sn*cx - cs*cy}
}

func loadAmber(path string) (scene.Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return scene.Document{}, err
	}
	var env struct {
		Document scene.Document `json:"document"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return scene.Document{}, err
	}
	return env.Document, nil
}

// sceneSet builds the coverage/density matrix. All scenes stay inside the
// production admission budget (segments*W*H*4 <= 1.6e8).
func sceneSet(amberPath string) ([]scene.Document, []string, error) {
	var docs []scene.Document
	var names []string
	add := func(name string, d scene.Document) {
		docs = append(docs, d)
		names = append(names, name)
	}

	amber, err := loadAmber(amberPath)
	if err != nil {
		return nil, nil, fmt.Errorf("amber: %w", err)
	}
	add("amber-192x144(real)", amber)

	// Sparse large canvas: two small shapes on 512x512.
	add("sparse-512", scene.Document{
		Width: 512, Height: 512,
		Background: scene.Color{R: 0.05, G: 0.05, B: 0.08, A: 1},
		Layers: []scene.Layer{{
			ID: "l1", Opacity: 1, Blend: "normal",
			Shapes: []scene.Shape{
				quad("a", 40, 40, 24, 24, solid(0.9, 0.3, 0.2, 0.8), geom.Identity),
				quad("b", 420, 430, 24, 24, gradient(420, 430, 444, 454,
					scene.Color{R: 0.1, G: 0.6, B: 0.9, A: 0.9}, scene.Color{R: 0.9, G: 0.9, B: 0.2, A: 0.4}), geom.Identity),
			},
		}},
	})

	// Coverage sweep on 512x512: 36 quads (144 segments) sized for ~10/50/90%,
	// split 18/18 over two layers to stay inside the 32-shapes-per-layer limit.
	for _, cfg := range []struct {
		name string
		size float64
	}{{"cover10-512", 27}, {"cover50-512", 61}, {"cover90-512", 82}} {
		r := &lcg{s: 1447}
		var shapes []scene.Shape
		for i := 0; i < 36; i++ {
			gx := float64(i%6) * 85.3
			gy := float64(i/6) * 85.3
			jx := (r.next() - 0.5) * 8
			jy := (r.next() - 0.5) * 8
			shapes = append(shapes, quad(fmt.Sprintf("q%d", i), gx+jx, gy+jy, cfg.size, cfg.size,
				solid(0.2+0.6*r.next(), 0.2+0.6*r.next(), 0.2+0.6*r.next(), 0.55+0.4*r.next()), geom.Identity))
		}
		add(cfg.name, scene.Document{
			Width: 512, Height: 512,
			Background: scene.Color{R: 0.06, G: 0.06, B: 0.1, A: 1},
			Layers: []scene.Layer{
				{ID: "l1", Opacity: 1, Blend: "normal", Shapes: shapes[:18]},
				{ID: "l2", Opacity: 0.85, Blend: "normal", Shapes: shapes[18:]},
			},
		})
	}

	// Dense transparent overlap: 24 rotated quads stacked on the same region,
	// split over 3 layers exercising group opacity + multiply/screen.
	{
		r := &lcg{s: 991}
		blends := []struct {
			id    string
			blend string
			op    float64
		}{{"g-normal", "normal", 0.7}, {"g-multiply", "multiply", 1}, {"g-screen", "screen", 0.5}}
		var layers []scene.Layer
		for k, cfg := range blends {
			var shapes []scene.Shape
			for i := 0; i < 8; i++ {
				th := (float64(k*8+i) / 24) * math.Pi / 3
				cx := 256 + (r.next()-0.5)*30
				cy := 256 + (r.next()-0.5)*30
				var br scene.Brush
				if i%2 == 0 {
					br = gradient(-160, -160, 160, 160,
						scene.Color{R: r.next(), G: r.next(), B: r.next(), A: 0.75},
						scene.Color{R: r.next(), G: r.next(), B: r.next(), A: 0.35})
				} else {
					br = solid(r.next(), r.next(), r.next(), 0.4+0.5*r.next())
				}
				sh := quad(fmt.Sprintf("s%d_%d", k, i), -160, -160, 320, 320, br, geom.Identity)
				sh.Transform = rotate(th, 0, 0)
				// translate to canvas center via matrix composition: rotate then translate
				sh.Transform[4] += cx
				sh.Transform[5] += cy
				shapes = append(shapes, sh)
			}
			layers = append(layers, scene.Layer{ID: cfg.id, Opacity: cfg.op, Blend: cfg.blend, Shapes: shapes})
		}
		add("dense-stack-512", scene.Document{
			Width: 512, Height: 512,
			Background: scene.Color{R: 0.09, G: 0.08, B: 0.07, A: 1},
			Layers:     layers,
		})
	}

	// Boundary stress: 16 long thin rotated bars crossing the whole canvas.
	{
		var shapes []scene.Shape
		for i := 0; i < 16; i++ {
			cx := 32 + float64(i)*30
			sh := quad(fmt.Sprintf("bar%d", i), -4, -280, 8, 560,
				solid(0.3+0.04*float64(i), 0.7-0.02*float64(i), 0.5, 0.6), geom.Identity)
			sh.Transform = rotate(math.Pi/7, 0, 0)
			sh.Transform[4] += cx
			sh.Transform[5] += 256
			shapes = append(shapes, sh)
		}
		add("boundary-512", scene.Document{
			Width: 512, Height: 512,
			Background: scene.Color{R: 0.05, G: 0.05, B: 0.05, A: 1},
			Layers:     []scene.Layer{{ID: "l1", Opacity: 1, Blend: "normal", Shapes: shapes}},
		})
	}

	// evenodd hole + clip + mask + gradient + screen on 256x256.
	add("evenodd-clip-256", scene.Document{
		Width: 256, Height: 256,
		Background: scene.Color{R: 0.08, G: 0.07, B: 0.12, A: 1},
		Layers: []scene.Layer{{
			ID: "l1", Opacity: 0.6, Blend: "screen",
			Clip: []geom.Op{
				{Kind: "M", Points: []geom.Point{{X: 128, Y: 8}}},
				{Kind: "L", Points: []geom.Point{{X: 248, Y: 128}}},
				{Kind: "L", Points: []geom.Point{{X: 128, Y: 248}}},
				{Kind: "L", Points: []geom.Point{{X: 8, Y: 128}}},
				{Kind: "Z"},
			},
			Mask: []geom.Op{
				{Kind: "M", Points: []geom.Point{{X: 24, Y: 24}}},
				{Kind: "L", Points: []geom.Point{{X: 232, Y: 24}}},
				{Kind: "L", Points: []geom.Point{{X: 232, Y: 232}}},
				{Kind: "L", Points: []geom.Point{{X: 24, Y: 232}}},
				{Kind: "Z"},
			},
			Shapes: []scene.Shape{
				{
					ID: "ring", FillRule: "evenodd",
					Path: []geom.Op{
						{Kind: "M", Points: []geom.Point{{X: 40, Y: 40}}},
						{Kind: "L", Points: []geom.Point{{X: 216, Y: 40}}},
						{Kind: "L", Points: []geom.Point{{X: 216, Y: 216}}},
						{Kind: "L", Points: []geom.Point{{X: 40, Y: 216}}},
						{Kind: "Z"},
						{Kind: "M", Points: []geom.Point{{X: 88, Y: 88}}},
						{Kind: "L", Points: []geom.Point{{X: 168, Y: 88}}},
						{Kind: "L", Points: []geom.Point{{X: 168, Y: 168}}},
						{Kind: "L", Points: []geom.Point{{X: 88, Y: 168}}},
						{Kind: "Z"},
					},
					Brush: gradient(40, 40, 216, 216,
						scene.Color{R: 0.9, G: 0.4, B: 0.2, A: 0.9}, scene.Color{R: 0.2, G: 0.5, B: 0.95, A: 0.5}),
					Transform: geom.Identity,
				},
				quad("back", 100, 60, 120, 150, solid(0.5, 0.9, 0.4, 0.45), geom.Identity),
			},
		}},
	})

	// Small canvases where full-layer offscreen fits the 8MiB budget.
	add("small-128", scene.Document{
		Width: 128, Height: 128,
		Background: scene.Color{R: 0.1, G: 0.09, B: 0.08, A: 1},
		Layers: []scene.Layer{
			{ID: "l1", Opacity: 0.8, Blend: "multiply", Shapes: []scene.Shape{
				quad("a", 16, 16, 64, 64, solid(0.8, 0.3, 0.3, 0.7), geom.Identity),
				quad("b", 48, 48, 64, 64, gradient(48, 48, 112, 112,
					scene.Color{R: 0.2, G: 0.7, B: 0.9, A: 0.8}, scene.Color{R: 0.9, G: 0.8, B: 0.2, A: 0.5}), geom.Identity),
			}},
			{ID: "l2", Opacity: 1, Blend: "normal", Shapes: []scene.Shape{
				quad("c", 60, 20, 40, 90, solid(0.3, 0.8, 0.5, 0.6), rotate(math.Pi/9, 80, 65)),
			}},
		},
	})
	add("tall-128x256", scene.Document{
		Width: 128, Height: 256,
		Background: scene.Color{R: 0.07, G: 0.07, B: 0.09, A: 1},
		Layers: []scene.Layer{{ID: "l1", Opacity: 1, Blend: "normal", Shapes: []scene.Shape{
			quad("a", 20, 30, 88, 60, solid(0.7, 0.5, 0.2, 0.8), geom.Identity),
			quad("b", 30, 120, 70, 100, solid(0.2, 0.6, 0.8, 0.7), geom.Identity),
		}}},
	})
	return docs, names, nil
}
