// Package main evaluates compositing schedules for prism-plate without
// touching production code. S0 is an instrumented clone of raster.Render;
// S1/S2/S3 are candidate schedules whose PNG bytes are verified against
// the production renderer on every scene.
package main

import (
	"image"
	"image/color"
	"math"
	"prismplate/internal/geom"
	"prismplate/internal/paint"
	"prismplate/internal/scene"
	"sort"
)

// Counters tallies the primitive costs shared by all strategies so that
// compute cost can be compared independently of wall time.
type Counters struct {
	EdgeIters int64 // edge loop iterations in coverage tests
	CrossX    int64 // crossing x computations
	Contains  int64 // point-in-path calls
	Samples   int64 // brush samples (covered hits)
	Overs     int64 // compositing operations
	Scales    int64 // group-opacity scales
}

// containsCount mirrors geom.Path.Contains exactly, including the half-open
// edge rule and the strict x > p.X test, so coverage decisions are bit-identical.
func containsCount(path geom.Path, p geom.Point, rule string, ct *Counters) bool {
	winding := 0
	crossings := 0
	for _, ring := range path {
		for i, a := range ring {
			b := ring[(i+1)%len(ring)]
			ct.EdgeIters++
			if (a.Y <= p.Y && b.Y > p.Y) || (b.Y <= p.Y && a.Y > p.Y) {
				x := a.X + (p.Y-a.Y)*(b.X-a.X)/(b.Y-a.Y)
				ct.CrossX++
				if x > p.X {
					crossings++
					if b.Y > a.Y {
						winding++
					} else {
						winding--
					}
				}
			}
		}
	}
	if rule == "evenodd" {
		return crossings%2 == 1
	}
	return winding != 0
}

func emit(img *image.NRGBA, x, y int, sum paint.Pixel) {
	r, g, b, a := sum.RGBA8()
	img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
}

// renderS0 is an instrumented, line-faithful clone of raster.Render
// (per-sample-point evaluation, no intermediate buffers).
func renderS0(c scene.Compiled, ct *Counters) *image.NRGBA {
	d := c.Document
	img := image.NewNRGBA(image.Rect(0, 0, d.Width, d.Height))
	for y := 0; y < d.Height; y++ {
		for x := 0; x < d.Width; x++ {
			sum := paint.Pixel{}
			for sy := 0; sy < 2; sy++ {
				for sx := 0; sx < 2; sx++ {
					p := geom.Point{X: float64(x) + (float64(sx)+0.5)/2, Y: float64(y) + (float64(sy)+0.5)/2}
					dst := paint.From(d.Background)
					for _, l := range c.Layers {
						if len(l.Clip) > 0 {
							ct.Contains++
							if !containsCount(l.Clip, p, "nonzero", ct) {
								continue
							}
						}
						if len(l.Mask) > 0 {
							ct.Contains++
							if !containsCount(l.Mask, p, "nonzero", ct) {
								continue
							}
						}
						src := paint.Pixel{}
						for _, s := range l.Shapes {
							q := s.Inverse.Apply(p)
							ct.Contains++
							if containsCount(s.Path, q, s.Shape.FillRule, ct) {
								ct.Samples++
								src = paint.Over(src, paint.Sample(s.Shape.Brush, q), "normal")
								ct.Overs++
							}
						}
						ct.Scales++
						dst = paint.Over(dst, src.Scale(l.Layer.Opacity), l.Layer.Blend)
						ct.Overs++
					}
					sum.R += dst.R / 4
					sum.G += dst.G / 4
					sum.B += dst.B / 4
					sum.A += dst.A / 4
				}
			}
			emit(img, x, y, sum)
		}
	}
	return img
}

// renderS1 composites each whole layer into an offscreen sample-resolution
// buffer, then folds it into a full-canvas destination buffer. Per-sample
// operation order is identical to S0, so results are bit-identical; the cost
// is two (2W)x(2H) float64 RGBA buffers.
func renderS1(c scene.Compiled, ct *Counters) (*image.NRGBA, int64) {
	d := c.Document
	W, H := d.Width, d.Height
	sw, sh := 2*W, 2*H
	dst := make([]paint.Pixel, sw*sh)
	src := make([]paint.Pixel, sw*sh)
	bufBytes := int64(2*sw*sh) * 32
	bg := paint.From(d.Background)
	for i := range dst {
		dst[i] = bg
	}
	for _, l := range c.Layers {
		for i := range src {
			src[i] = paint.Pixel{}
		}
		for sy := 0; sy < sh; sy++ {
			py := (float64(sy) + 0.5) / 2
			for sx := 0; sx < sw; sx++ {
				p := geom.Point{X: (float64(sx) + 0.5) / 2, Y: py}
				idx := sy*sw + sx
				if len(l.Clip) > 0 {
					ct.Contains++
					if !containsCount(l.Clip, p, "nonzero", ct) {
						continue
					}
				}
				if len(l.Mask) > 0 {
					ct.Contains++
					if !containsCount(l.Mask, p, "nonzero", ct) {
						continue
					}
				}
				acc := paint.Pixel{}
				for _, s := range l.Shapes {
					q := s.Inverse.Apply(p)
					ct.Contains++
					if containsCount(s.Path, q, s.Shape.FillRule, ct) {
						ct.Samples++
						acc = paint.Over(acc, paint.Sample(s.Shape.Brush, q), "normal")
						ct.Overs++
					}
				}
				src[idx] = acc
			}
		}
		// Samples skipped by clip/mask left src == 0, and Over(dst, 0, mode)
		// is exactly dst (src.A == 0 selects the normal branch, d*1 == d),
		// so a uniform fold is bit-identical to S0's per-sample continue.
		for i := range dst {
			ct.Scales++
			dst[i] = paint.Over(dst[i], src[i].Scale(l.Layer.Opacity), l.Layer.Blend)
			ct.Overs++
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			sum := paint.Pixel{}
			for _, idx := range [4]int{(2*y)*sw + 2*x, (2*y)*sw + 2*x + 1, (2*y+1)*sw + 2*x, (2*y+1)*sw + 2*x + 1} {
				sum.R += dst[idx].R / 4
				sum.G += dst[idx].G / 4
				sum.B += dst[idx].B / 4
				sum.A += dst[idx].A / 4
			}
			emit(img, x, y, sum)
		}
	}
	return img, bufBytes
}

type rect struct{ Min, Max geom.Point }

func pathBBox(p geom.Path) rect {
	r := rect{Min: geom.Point{X: math.Inf(1), Y: math.Inf(1)}, Max: geom.Point{X: math.Inf(-1), Y: math.Inf(-1)}}
	for _, ring := range p {
		for _, v := range ring {
			r.Min.X = math.Min(r.Min.X, v.X)
			r.Min.Y = math.Min(r.Min.Y, v.Y)
			r.Max.X = math.Max(r.Max.X, v.X)
			r.Max.Y = math.Max(r.Max.Y, v.Y)
		}
	}
	return r
}
func unionRect(a, b rect) rect {
	return rect{geom.Point{X: math.Min(a.Min.X, b.Min.X), Y: math.Min(a.Min.Y, b.Min.Y)}, geom.Point{X: math.Max(a.Max.X, b.Max.X), Y: math.Max(a.Max.Y, b.Max.Y)}}
}
func intersectRect(a, b rect) rect {
	r := rect{geom.Point{X: math.Max(a.Min.X, b.Min.X), Y: math.Max(a.Min.Y, b.Min.Y)}, geom.Point{X: math.Min(a.Max.X, b.Max.X), Y: math.Min(a.Max.Y, b.Max.Y)}}
	if r.Min.X > r.Max.X || r.Min.Y > r.Max.Y {
		return rect{Min: geom.Point{X: 1, Y: 1}, Max: geom.Point{X: 0, Y: 0}}
	}
	return r
}
func emptyRect(r rect) bool { return r.Min.X > r.Max.X || r.Min.Y > r.Max.Y }
func hitRect(a, b rect) bool {
	return a.Min.X <= b.Max.X && b.Min.X <= a.Max.X && a.Min.Y <= b.Max.Y && b.Min.Y <= a.Max.Y
}

// xformBBox maps a local bbox through an affine matrix; the image of the bbox
// corners bounds the image of every shape point.
func xformBBox(r rect, m geom.Matrix) rect {
	out := rect{Min: geom.Point{X: math.Inf(1), Y: math.Inf(1)}, Max: geom.Point{X: math.Inf(-1), Y: math.Inf(-1)}}
	for _, cx := range [2]float64{r.Min.X, r.Max.X} {
		for _, cy := range [2]float64{r.Min.Y, r.Max.Y} {
			p := m.Apply(geom.Point{X: cx, Y: cy})
			out.Min.X = math.Min(out.Min.X, p.X)
			out.Min.Y = math.Min(out.Min.Y, p.Y)
			out.Max.X = math.Max(out.Max.X, p.X)
			out.Max.Y = math.Max(out.Max.Y, p.Y)
		}
	}
	return out
}

type layerInfo struct {
	skip       bool // opacity == 0 or empty affected region: exact identity
	rect       rect // canvas region the layer can affect
	shapeRects []rect
}

// layerRects derives conservative culling regions. A sample outside rect is
// either clip/mask-excluded or uncovered by every shape, so the layer folds
// to identity there (Over(dst, 0, mode) == dst exactly).
func layerRects(c scene.Compiled) []layerInfo {
	infos := make([]layerInfo, len(c.Layers))
	for i, l := range c.Layers {
		li := layerInfo{}
		if l.Layer.Opacity == 0 {
			li.skip = true
			infos[i] = li
			continue
		}
		u := rect{Min: geom.Point{X: math.Inf(1), Y: math.Inf(1)}, Max: geom.Point{X: math.Inf(-1), Y: math.Inf(-1)}}
		for _, s := range l.Shapes {
			sr := xformBBox(pathBBox(s.Path), s.Shape.Transform)
			li.shapeRects = append(li.shapeRects, sr)
			u = unionRect(u, sr)
		}
		if len(l.Clip) > 0 {
			u = intersectRect(u, pathBBox(l.Clip))
		}
		if len(l.Mask) > 0 {
			u = intersectRect(u, pathBBox(l.Mask))
		}
		li.rect = u
		li.skip = emptyRect(u)
		infos[i] = li
	}
	return infos
}

// renderS2 evaluates per-sample inside tiles, culling layers and shapes whose
// canvas bbox misses the tile. It allocates no float buffers; every skipped
// test is a provably-false Contains, so output is bit-identical to S0.
func renderS2(c scene.Compiled, tile int, ct *Counters) *image.NRGBA {
	d := c.Document
	infos := layerRects(c)
	img := image.NewNRGBA(image.Rect(0, 0, d.Width, d.Height))
	bgPx := paint.From(d.Background)
	for ty := 0; ty < d.Height; ty += tile {
		th := min(tile, d.Height-ty)
		for tx := 0; tx < d.Width; tx += tile {
			tw := min(tile, d.Width-tx)
			tr := rect{geom.Point{X: float64(tx), Y: float64(ty)}, geom.Point{X: float64(tx + tw), Y: float64(ty + th)}}
			type relLayer struct {
				l      scene.CompiledLayer
				shapes []scene.CompiledShape
			}
			var rel []relLayer
			for li, l := range c.Layers {
				if infos[li].skip || !hitRect(tr, infos[li].rect) {
					continue
				}
				rl := relLayer{l: l}
				for si, s := range l.Shapes {
					if hitRect(tr, infos[li].shapeRects[si]) {
						rl.shapes = append(rl.shapes, s)
					}
				}
				rel = append(rel, rl)
			}
			if len(rel) == 0 {
				r, g, b, a := bgPx.RGBA8()
				for y := ty; y < ty+th; y++ {
					for x := tx; x < tx+tw; x++ {
						img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
					}
				}
				continue
			}
			for y := ty; y < ty+th; y++ {
				for x := tx; x < tx+tw; x++ {
					sum := paint.Pixel{}
					for sy := 0; sy < 2; sy++ {
						for sx := 0; sx < 2; sx++ {
							p := geom.Point{X: float64(x) + (float64(sx)+0.5)/2, Y: float64(y) + (float64(sy)+0.5)/2}
							dst := paint.From(d.Background)
							for _, rl := range rel {
								l := rl.l
								if len(l.Clip) > 0 {
									ct.Contains++
									if !containsCount(l.Clip, p, "nonzero", ct) {
										continue
									}
								}
								if len(l.Mask) > 0 {
									ct.Contains++
									if !containsCount(l.Mask, p, "nonzero", ct) {
										continue
									}
								}
								src := paint.Pixel{}
								for _, s := range rl.shapes {
									q := s.Inverse.Apply(p)
									ct.Contains++
									if containsCount(s.Path, q, s.Shape.FillRule, ct) {
										ct.Samples++
										src = paint.Over(src, paint.Sample(s.Shape.Brush, q), "normal")
										ct.Overs++
									}
								}
								ct.Scales++
								dst = paint.Over(dst, src.Scale(l.Layer.Opacity), l.Layer.Blend)
								ct.Overs++
							}
							sum.R += dst.R / 4
							sum.G += dst.G / 4
							sum.B += dst.B / 4
							sum.A += dst.A / 4
						}
					}
					emit(img, x, y, sum)
				}
			}
		}
	}
	return img
}

type crossing struct {
	x   float64
	dir int
}

// spanCrossings computes every edge crossing of a path with the horizontal
// line y, using the same float expression and half-open rule as geom.Contains.
func spanCrossings(path geom.Path, y float64, buf []crossing, ct *Counters) []crossing {
	buf = buf[:0]
	for _, ring := range path {
		for i, a := range ring {
			b := ring[(i+1)%len(ring)]
			ct.EdgeIters++
			if (a.Y <= y && b.Y > y) || (b.Y <= y && a.Y > y) {
				x := a.X + (y-a.Y)*(b.X-a.X)/(b.Y-a.Y)
				ct.CrossX++
				d := -1
				if b.Y > a.Y {
					d = 1
				}
				buf = append(buf, crossing{x, d})
			}
		}
	}
	sort.Slice(buf, func(i, j int) bool { return buf[i].x < buf[j].x })
	return buf
}

// windSum accumulates integer winding contributions (exact arithmetic).
func windSum(cr []crossing) int {
	w := 0
	for _, c := range cr {
		w += c.dir
	}
	return w
}

// renderS3 composites layer-by-layer inside tiles, using scanline spans for
// coverage. Clip/mask spans reuse the production canvas-space expression and
// are bit-exact; shape paths are pre-transformed to canvas space, which is
// mathematically equivalent to testing the inverse-mapped point but is only
// empirically bit-identical (verified per scene by the harness). With
// hoistSolid, constant solid brushes are evaluated once per shape per render
// (paint.Sample is a pure function of the constant color: bit-exact reuse).
func renderS3(c scene.Compiled, tile int, ct *Counters, hoistSolid bool) (*image.NRGBA, int64) {
	d := c.Document
	infos := layerRects(c)
	// Pre-transform shape paths to canvas space once per render.
	canvasPaths := make([][]geom.Path, len(c.Layers))
	solidPx := make([][]paint.Pixel, len(c.Layers))
	isSolid := make([][]bool, len(c.Layers))
	for li, l := range c.Layers {
		for _, s := range l.Shapes {
			var cp geom.Path
			for _, ring := range s.Path {
				var nr geom.Contour
				for _, v := range ring {
					nr = append(nr, s.Shape.Transform.Apply(v))
				}
				cp = append(cp, nr)
			}
			canvasPaths[li] = append(canvasPaths[li], cp)
			ok := hoistSolid && s.Shape.Brush.Kind == "solid"
			isSolid[li] = append(isSolid[li], ok)
			if ok {
				solidPx[li] = append(solidPx[li], paint.Sample(s.Shape.Brush, geom.Point{}))
			} else {
				solidPx[li] = append(solidPx[li], paint.Pixel{})
			}
		}
	}
	sn := 2 * tile
	dst := make([]paint.Pixel, sn*sn)
	src := make([]paint.Pixel, sn*sn)
	active := make([]bool, sn*sn)
	bufBytes := int64(2*sn*sn)*32 + int64(sn*sn)
	var crbuf, crbuf2 []crossing
	img := image.NewNRGBA(image.Rect(0, 0, d.Width, d.Height))
	bg := paint.From(d.Background)
	for ty := 0; ty < d.Height; ty += tile {
		th := min(tile, d.Height-ty)
		for tx := 0; tx < d.Width; tx += tile {
			tw := min(tile, d.Width-tx)
			sw, sh := 2*tw, 2*th
			n := sw * sh
			tr := rect{geom.Point{X: float64(tx), Y: float64(ty)}, geom.Point{X: float64(tx + tw), Y: float64(ty + th)}}
			for i := 0; i < n; i++ {
				dst[i] = bg
			}
			for li, l := range c.Layers {
				if infos[li].skip || !hitRect(tr, infos[li].rect) {
					continue
				}
				hasClip, hasMask := len(l.Clip) > 0, len(l.Mask) > 0
				for i := 0; i < n; i++ {
					active[i] = !hasClip && !hasMask
					src[i] = paint.Pixel{}
				}
				if hasClip || hasMask {
					// One crossing sweep per scanline; samples advance two
					// pointers. Production tests clip/mask with "nonzero".
					for sy := 0; sy < sh; sy++ {
						py := float64(ty) + (float64(sy)+0.5)/2
						var crC, crM []crossing
						remC, remM := 0, 0
						if hasClip {
							crC = spanCrossings(l.Clip, py, crbuf, ct)
							remC = windSum(crC)
						}
						if hasMask {
							crM = spanCrossings(l.Mask, py, crbuf2, ct)
							remM = windSum(crM)
						}
						ciC, ciM := 0, 0
						for sx := 0; sx < sw; sx++ {
							px := float64(tx) + (float64(sx)+0.5)/2
							ok := true
							if hasClip {
								for ciC < len(crC) && crC[ciC].x <= px {
									remC -= crC[ciC].dir
									ciC++
								}
								ok = remC != 0
							}
							if ok && hasMask {
								for ciM < len(crM) && crM[ciM].x <= px {
									remM -= crM[ciM].dir
									ciM++
								}
								ok = remM != 0
							}
							active[sy*sw+sx] = ok
						}
					}
				}
				for si, s := range l.Shapes {
					sr := infos[li].shapeRects[si]
					if !hitRect(tr, sr) {
						continue
					}
					cp := canvasPaths[li][si]
					cb := pathBBox(cp)
					sy0 := max(0, min(sh, int(math.Floor(2*(cb.Min.Y-float64(ty))-0.5))))
					sy1 := max(0, min(sh, int(math.Ceil(2*(cb.Max.Y-float64(ty))+0.5))))
					sx0 := max(0, min(sw, int(math.Floor(2*(cb.Min.X-float64(tx))-0.5))))
					sx1 := max(0, min(sw, int(math.Ceil(2*(cb.Max.X-float64(tx))+0.5))))
					for sy := sy0; sy < sy1; sy++ {
						py := float64(ty) + (float64(sy)+0.5)/2
						cr := spanCrossings(cp, py, crbuf, ct)
						if len(cr) == 0 {
							continue
						}
						// remaining winding/count after removing crossings <= px
						rem := len(cr)
						wind := windSum(cr)
						ci := 0
						for sx := sx0; sx < sx1; sx++ {
							px := float64(tx) + (float64(sx)+0.5)/2
							for ci < len(cr) && cr[ci].x <= px {
								rem--
								wind -= cr[ci].dir
								ci++
							}
							inside := wind != 0
							if s.Shape.FillRule == "evenodd" {
								inside = rem%2 == 1
							}
							idx := sy*sw + sx
							if !inside || !active[idx] {
								continue
							}
							var sp paint.Pixel
							if isSolid[li][si] {
								sp = solidPx[li][si]
							} else {
								p := geom.Point{X: px, Y: py}
								ct.Samples++
								sp = paint.Sample(s.Shape.Brush, s.Inverse.Apply(p))
							}
							src[idx] = paint.Over(src[idx], sp, "normal")
							ct.Overs++
						}
					}
				}
				for i := 0; i < n; i++ {
					if !active[i] || src[i].A == 0 {
						continue
					}
					ct.Scales++
					dst[i] = paint.Over(dst[i], src[i].Scale(l.Layer.Opacity), l.Layer.Blend)
					ct.Overs++
				}
			}
			for y := 0; y < th; y++ {
				for x := 0; x < tw; x++ {
					sum := paint.Pixel{}
					for _, idx := range [4]int{(2*y)*sw + 2*x, (2*y)*sw + 2*x + 1, (2*y+1)*sw + 2*x, (2*y+1)*sw + 2*x + 1} {
						sum.R += dst[idx].R / 4
						sum.G += dst[idx].G / 4
						sum.B += dst[idx].B / 4
						sum.A += dst[idx].A / 4
					}
					emit(img, tx+x, ty+y, sum)
				}
			}
		}
	}
	return img, bufBytes
}
