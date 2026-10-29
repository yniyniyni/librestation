// Package anim renders single frames of the LED ring and screen animations.
// Every function is pure: the same inputs give the same output. The shapes,
// speeds and formulas are ported from the design mockups.
package anim

import (
	"math"
	"time"

	"voice/internal/led/asset"
)

// Ring is the 24 ring LEDs. Index 0 is the LED at the front of the speaker
// (nearest the user); indexes increase going front, right, back, left.
type Ring [24]asset.RGB

const ringN = 24

type rgbF [3]float64

// Google colors: blue, red, yellow, green.
var google = [4]rgbF{{66, 133, 244}, {234, 67, 53}, {251, 188, 5}, {52, 168, 83}}

// stockVolume is the stock volume ring color for k lit LEDs (index k-1).
var stockVolume = [23]string{
	"000500", "009A45", "00D345", "11DD45", "1DE545", "29EC45", "35F345", "42FB45",
	"5CFF45", "6DFF45", "80FF45", "9AFF45", "AFFF45", "C4FF45", "DBFF45", "F1FF45",
	"FFEF3C", "FFC927", "FF9F10", "FB7F06", "F55500", "EB2A00", "E30900",
}

func hexRGB(s string) asset.RGB {
	var v [3]uint8
	for i := range v {
		for _, c := range s[i*2 : i*2+2] {
			d := uint8(0)
			switch {
			case c >= '0' && c <= '9':
				d = uint8(c - '0')
			case c >= 'A' && c <= 'F':
				d = uint8(c-'A') + 10
			}
			v[i] = v[i]<<4 | d
		}
	}
	return asset.RGB{R: v[0], G: v[1], B: v[2]}
}

func round(x float64) int { return int(math.Floor(x + 0.5)) }

func clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }

func toRGB(c rgbF) asset.RGB {
	f := func(x float64) uint8 { return uint8(math.Max(0, math.Min(255, float64(round(x))))) }
	return asset.RGB{R: f(c[0]), G: f(c[1]), B: f(c[2])}
}

func scale(c rgbF, k float64) rgbF { return rgbF{c[0] * k, c[1] * k, c[2] * k} }

// mixBlack is mix([0,0,0], c, k) with per-channel rounding.
func mixBlack(c rgbF, k float64) asset.RGB { return toRGB(scale(c, k)) }

func mix(a, b rgbF, t float64) rgbF {
	var o rgbF
	for i := range o {
		o[i] = float64(round(a[i] + (b[i]-a[i])*t))
	}
	return o
}

// googleAt walks the four Google colors as a loop, u in turns.
func googleAt(u float64) rgbF {
	u -= math.Floor(u)
	i := int(math.Floor(u * 4))
	return mix(google[i%4], google[(i+1)%4], u*4-float64(i))
}

func posMod(x, m float64) float64 { return x - m*math.Floor(x/m) }

// VolumeRing is the stock volume ring: k=round(v/100*23) LEDs from index 0.
func VolumeRing(v int) Ring {
	var r Ring
	v = max(0, min(100, v))
	k := round(float64(v) / 100 * 23)
	for i := 0; i < k; i++ {
		r[i] = hexRGB(stockVolume[k-1])
	}
	return r
}

// AIRing is the "assistant is listening" ring. Styles: "carousel", "flow",
// "stock"; any other style is dark.
func AIRing(style string, t time.Duration) Ring {
	var r Ring
	s := t.Seconds()
	for i := 0; i < ringN; i++ {
		fi := float64(i)
		switch style {
		case "carousel":
			r[i] = toRGB(google[int(posMod(fi-s*12, ringN)/6)%4])
		case "flow":
			r[i] = mixBlack(googleAt(fi/ringN+s*0.25), 0.75+0.25*math.Sin(s*3))
		case "stock":
			d := float64(min(i, ringN-i))
			w := 6 + 4*math.Sin(s*2.5)
			r[i] = mixBlack(googleAt(d/ringN+s*0.4), clamp01(w-d))
		}
	}
	return r
}

var actionColor = map[string]rgbF{
	"pause": {255, 170, 0},
	"play":  {40, 220, 80},
	"next":  {66, 133, 244},
	"prev":  {160, 90, 255},
}

// FlashRing is the short flash after a media command. Styles: "sweep",
// "white", "color"; actions: "pause", "play", "next", "prev". ok is false
// once the flash is over (or for an unknown style/action), with a dark ring.
func FlashRing(style, action string, t time.Duration) (Ring, bool) {
	var r Ring
	col, known := actionColor[action]
	if !known || t < 0 {
		return r, false
	}
	s := t.Seconds()
	switch style {
	case "white":
		if s >= 0.4 {
			return r, false
		}
		c := toRGB(scale(rgbF{255, 255, 255}, 1-s/0.4))
		for i := range r {
			r[i] = c
		}
	case "color":
		if s >= 0.6 {
			return r, false
		}
		if int(s/0.15)%2 == 0 {
			c := toRGB(col)
			for i := range r {
				r[i] = c
			}
		}
	case "sweep":
		if s >= 0.6 {
			return r, false
		}
		p := s / 0.5
		for i := range r {
			fi := float64(i)
			var d float64
			switch action {
			case "next":
				d = math.Abs(fi/ringN - p)
			case "prev":
				d = math.Abs(math.Mod(ringN-fi, ringN)/ringN - p)
			default:
				m := float64(min(i, ringN-i)) / 12
				if action == "pause" {
					d = math.Abs((1 - m) - p)
				} else {
					d = math.Abs(m - p)
				}
			}
			if k := 1 - d*6; k > 0 {
				r[i] = toRGB(scale(col, k))
			}
		}
	default:
		return r, false
	}
	return r, true
}
