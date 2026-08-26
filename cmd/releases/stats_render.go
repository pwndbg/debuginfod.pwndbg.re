package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"time"
)

// statsCSP pins the one inline script by hash. Every bar label on this page is a
// string a client chose - a filename, a version, a country header - so escaping is
// what keeps them inert, and this is the second line if escaping is ever missed:
// a hash source blocks injected <script> AND inline event handlers, neither of
// which can match. A nonce would not, since the page is cached and served
// unchanged for an hour, long enough to read the nonce back out and reuse it.
//
// style-src stays 'unsafe-inline': the bars set their width through a style
// attribute, so there is nothing to pin.
var statsCSP = func() string {
	sum := sha256.Sum256([]byte(statsJS))
	return "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

const (
	panelW = 340
	panelH = 96
	padTop = 8
	padBot = 14
)

// Palette and layout are deliberately the same tokens cmd/proxy's /stats uses, so
// the two pages read as one system. They are duplicated rather than shared because
// the Dockerfiles copy cmd/ only - a shared package would have to be wired into
// all three images - and because the two pages have almost no markup in common:
// this one is mostly ranked bars, that one is a grid of small multiples.
const statsCSS = `
.viz-root{color-scheme:light;
 --surface-1:#fcfcfb;--plane:#f9f9f7;--text-primary:#0b0b0b;--text-secondary:#52514e;
 --muted:#898781;--grid:#e1e0d9;--axis:#c3c2b7;--border:rgba(11,11,11,.10);
 --served:#2a78d6;--band:#86b6ef;--ident:#1baf7a;--warn:#eda100;--err:#e34948;
 --r0:#2a78d6;--r1:#1baf7a;--r2:#eda100;--r3:#e34948;--r4:#898781}
@media (prefers-color-scheme:dark){:root:where(:not([data-theme="light"])) .viz-root{color-scheme:dark;
 --surface-1:#1a1a19;--plane:#0d0d0d;--text-primary:#fff;--text-secondary:#c3c2b7;
 --muted:#898781;--grid:#2c2c2a;--axis:#383835;--border:rgba(255,255,255,.10);
 --served:#3987e5;--band:#256abf;--ident:#199e70;--warn:#c98500;--err:#d03b3b;
 --r0:#3987e5;--r1:#199e70;--r2:#c98500;--r3:#d03b3b;--r4:#c3c2b7}}
:root[data-theme="dark"] .viz-root{color-scheme:dark;
 --surface-1:#1a1a19;--plane:#0d0d0d;--text-primary:#fff;--text-secondary:#c3c2b7;
 --muted:#898781;--grid:#2c2c2a;--axis:#383835;--border:rgba(255,255,255,.10);
 --served:#3987e5;--band:#256abf;--ident:#199e70;--warn:#c98500;--err:#d03b3b;
 --r0:#3987e5;--r1:#199e70;--r2:#c98500;--r3:#d03b3b;--r4:#c3c2b7}
*{box-sizing:border-box}
body{margin:0;font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
.viz-root{background:var(--plane);color:var(--text-primary);min-height:100vh;padding:28px 20px 56px}
.wrap{max-width:1160px;margin:0 auto}
h1{font-size:20px;margin:0 0 4px;letter-spacing:-.01em}
h2.sec{font-size:15px;margin:30px 0 4px;letter-spacing:-.01em}
.sub{color:var(--text-secondary);margin:0 0 18px;font-size:13px;max-width:88ch}
.head{display:flex;align-items:baseline;gap:14px;flex-wrap:wrap}
.views{margin-left:auto;display:flex;gap:2px;background:var(--surface-1);
 border:1px solid var(--border);border-radius:8px;padding:2px}
.views a,.views span{font-size:12.5px;padding:3px 10px;border-radius:6px;text-decoration:none;
 color:var(--text-secondary);line-height:1.4}
.views a:hover{background:var(--grid);color:var(--text-primary)}
.views .on{background:var(--served);color:#fff;font-weight:600}
.kpis{display:flex;flex-wrap:wrap;gap:10px;margin-bottom:18px}
.kpi{flex:1 1 150px;background:var(--surface-1);border:1px solid var(--border);border-radius:10px;padding:12px 14px}
.kpi .k{color:var(--text-secondary);font-size:12px}
.kpi .v{font-size:24px;font-weight:600;letter-spacing:-.02em;margin-top:2px}
.legend{display:flex;flex-wrap:wrap;gap:14px;align-items:center;margin:0 0 12px;font-size:12.5px;color:var(--text-secondary)}
.sw{display:inline-block;width:11px;height:11px;border-radius:3px;margin-right:6px;vertical-align:-1px}
.swl{display:inline-block;width:14px;height:0;border-top:2px solid var(--ident);margin-right:6px;vertical-align:4px}
.row2{display:grid;grid-template-columns:repeat(2,1fr);gap:12px}
.row3{display:grid;grid-template-columns:repeat(3,1fr);gap:12px}
@media (max-width:820px){.row2,.row3{grid-template-columns:1fr}}
.card{margin:0 0 12px;background:var(--surface-1);border:1px solid var(--border);border-radius:10px;padding:10px 12px 10px}
figcaption{display:flex;flex-wrap:wrap;align-items:baseline;gap:6px;margin-bottom:6px}
.epname{font-size:12.5px;font-weight:600;color:var(--text-primary)}
.hstat{margin-left:auto;color:var(--text-secondary);font-size:12px;font-variant-numeric:tabular-nums}
.hstat b{color:var(--text-primary);font-weight:600}
.plot{position:relative}
.spark{display:block;width:100%;height:96px}
.band{fill:var(--band)}
.ln{fill:none;stroke-width:1.8;vector-effect:non-scaling-stroke}
.c-ip{stroke:var(--ident)}
.c-gh{stroke:var(--warn)}
.c-r0{stroke:var(--r0)}.c-r1{stroke:var(--r1)}.c-r2{stroke:var(--r2)}
.c-r3{stroke:var(--r3)}.c-r4{stroke:var(--r4)}
.thr{stroke:var(--muted);stroke-width:1;stroke-dasharray:3 3;vector-effect:non-scaling-stroke}
.tall{height:150px}
.axis{stroke:var(--axis);stroke-width:1;vector-effect:non-scaling-stroke}
.cross{stroke:var(--text-secondary);stroke-width:1;vector-effect:non-scaling-stroke;opacity:.6}
.brow{display:flex;align-items:center;gap:10px;padding:2.5px 0}
.blab{flex:0 0 128px;font-size:12.5px;color:var(--text-secondary);overflow:hidden;
 text-overflow:ellipsis;white-space:nowrap}
.blab.wide{flex-basis:290px}
.btrack{flex:1;height:14px;background:var(--grid);border-radius:4px;overflow:hidden}
.bfill{display:block;height:100%;background:var(--served);border-radius:4px}
.bval{flex:0 0 104px;text-align:right;font-size:12.5px;font-variant-numeric:tabular-nums;color:var(--text-secondary)}
.bval b{color:var(--text-primary);font-weight:600}
.empty{color:var(--muted);font-size:12.5px;padding:6px 0}
.tip{position:absolute;pointer-events:none;background:var(--surface-1);border:1px solid var(--border);
 border-radius:8px;padding:6px 9px;font-size:12px;box-shadow:0 4px 14px rgba(0,0,0,.14);
 white-space:nowrap;z-index:5;font-variant-numeric:tabular-nums;line-height:1.45}
.tip i{display:inline-block;width:8px;height:8px;border-radius:2px;margin-right:5px}
.foot{color:var(--muted);font-size:11.5px;margin-top:22px;max-width:80ch}
`

const statsJS = `
document.querySelectorAll('.card[data-series]').forEach(card=>{
 const data=JSON.parse(card.dataset.series);
 const names=JSON.parse(card.dataset.labels), vars=JSON.parse(card.dataset.vars);
 const svg=card.querySelector('svg'), tip=card.querySelector('.tip');
 const cross=card.querySelector('.cross'), plot=card.querySelector('.plot');
 if(!data.length) return;
 const vbW=svg.viewBox.baseVal.width;
 plot.addEventListener('pointermove',e=>{
  const r=plot.getBoundingClientRect();
  const f=Math.min(1,Math.max(0,(e.clientX-r.left)/r.width));
  const i=Math.round(f*(data.length-1)), row=data[i];
  const frag=document.createDocumentFragment();
  const hd=document.createElement('b'); hd.textContent=row[0]; frag.append(hd);
  for(let k=0;k<names.length;k++){
   frag.append(document.createElement('br'));
   const sw=document.createElement('i'); sw.style.background='var('+vars[k]+')'; frag.append(sw);
   frag.append(document.createTextNode(names[k]+' '+row[k+1].toLocaleString('en')));
  }
  tip.replaceChildren(frag); tip.hidden=false;
  const x=(i/(data.length-1))*r.width;
  tip.style.left=Math.min(r.width-tip.offsetWidth-4,Math.max(0,x-tip.offsetWidth/2))+'px';
  tip.style.top='-6px';
  cross.style.display=''; const vx=(i/(data.length-1))*vbW;
  cross.setAttribute('x1',vx); cross.setAttribute('x2',vx);
 });
 plot.addEventListener('pointerleave',()=>{tip.hidden=true;cross.style.display='none'});
});
`

type svgGeom struct{ n int }

func (g svgGeom) x(i int) float64 {
	if g.n <= 1 {
		return 2
	}
	return 2 + float64(i)*float64(panelW-6)/float64(g.n-1)
}

func (g svgGeom) y(v, max float64) float64 {
	if max <= 0 {
		max = 1
	}
	if v > max {
		v = max
	}
	ph := float64(panelH - padTop - padBot)
	return padTop + ph - (v/max)*ph
}

func (g svgGeom) base() float64 { return g.y(0, 1) }

func esc(s string) string { return html.EscapeString(s) }

// seriesPanel draws one filled area and any number of overlaid lines against a
// shared Y scale.
func seriesPanel(area []float64, lines [][]float64, classes []string, aria string) string {
	g := svgGeom{n: len(area)}
	max := maxOf(area)
	for _, l := range lines {
		max = math.Max(max, maxOf(l))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %d %d" preserveAspectRatio="none" role="img" aria-label="%s">`,
		panelW, panelH, esc(aria))
	if anyPositive(area) {
		b.WriteString(`<polygon class="band" points="`)
		fmt.Fprintf(&b, "%.1f,%.1f ", g.x(0), g.base())
		for i, v := range area {
			fmt.Fprintf(&b, "%.1f,%.1f ", g.x(i), g.y(v, max))
		}
		fmt.Fprintf(&b, `%.1f,%.1f"/>`, g.x(g.n-1), g.base())
	}
	for k, l := range lines {
		if len(l) == 0 || !anyPositive(l) {
			continue
		}
		fmt.Fprintf(&b, `<path class="ln %s" d="M%.1f,%.1f`, classes[k], g.x(0), g.y(l[0], max))
		for i, v := range l {
			fmt.Fprintf(&b, "L%.1f,%.1f", g.x(i), g.y(v, max))
		}
		b.WriteString(`"/>`)
	}
	fmt.Fprintf(&b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
		g.x(0), g.base(), g.x(g.n-1), g.base())
	fmt.Fprintf(&b, `<line class="cross" x1="0" y1="%d" x2="0" y2="%.1f" style="display:none"/></svg>`,
		padTop, g.base())
	return b.String()
}

// curveClasses maps a release to a colour by recency, newest first. Two releases
// are drawn as two lines with a legend and the tag spelled out, so identity never
// rests on colour alone - which is what makes it acceptable that shipping a new
// release shifts the older ones one slot along. The alternative, a colour fixed
// per tag, would have to be arbitrary, and the reading that matters most here is
// "is the newest tracking above or below the ones before it" - so the newest
// keeping one predictable colour is worth more.
var curveClasses = []string{"c-r0", "c-r1", "c-r2", "c-r3", "c-r4"}
var curveVars = []string{"--r0", "--r1", "--r2", "--r3", "--r4"}

// curvesPanel draws cumulative downloads against days-since-release. Each line
// stops where its release stopped being sampled - that end point is information,
// not a gap to paper over.
func curvesPanel(curves []releaseCurve, n int, aria string) string {
	g := svgGeom{n: n}
	max := 0.0
	for _, c := range curves {
		max = math.Max(max, maxOf(c.Series))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark tall" viewBox="0 0 %d %d" preserveAspectRatio="none" role="img" aria-label="%s">`,
		panelW, panelH, esc(aria))
	for k, c := range curves {
		if len(c.Series) < 2 {
			continue
		}
		fmt.Fprintf(&b, `<path class="ln %s" d="M%.1f,%.1f`, curveClasses[k%len(curveClasses)],
			g.x(0), g.y(c.Series[0], max))
		for i, v := range c.Series {
			fmt.Fprintf(&b, "L%.1f,%.1f", g.x(i), g.y(v, max))
		}
		b.WriteString(`"/>`)
	}
	fmt.Fprintf(&b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
		g.x(0), g.base(), g.x(n-1), g.base())
	fmt.Fprintf(&b, `<line class="cross" x1="0" y1="%d" x2="0" y2="%.1f" style="display:none"/></svg>`,
		padTop, g.base())
	return b.String()
}

func anyPositive(v []float64) bool {
	for _, x := range v {
		if x > 0 {
			return true
		}
	}
	return false
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func toFloats(v []uint64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	return out
}

func seriesJSON(days []string, cols ...[]float64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, d := range days {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `["%s"`, d)
		for _, c := range cols {
			b.WriteByte(',')
			b.WriteString(strconv.FormatFloat(math.Round(c[i]*100)/100, 'f', -1, 64))
		}
		b.WriteByte(']')
	}
	b.WriteByte(']')
	return b.String()
}

// jsonStrings builds a JSON array for a data- attribute. The attribute is quoted
// with ', which html.EscapeString also escapes - so escaping here is what keeps a
// label containing a quote from closing the attribute early. Every caller passes
// literals today; this is so that stops being load-bearing.
func jsonStrings(items ...string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = `"` + esc(s) + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// chartCard renders one plot. `stat` is deliberately raw HTML - callers put <b>
// and entities in it - so anything variable inside it must already be escaped at
// the call site; `title` is escaped here, matching barCard.
func chartCard(b *strings.Builder, title, stat, svg, data, labels, vars string) {
	fmt.Fprintf(b, `<figure class="card" data-series='%s' data-labels='%s' data-vars='%s'>`, data, labels, vars)
	fmt.Fprintf(b, `<figcaption><span class="epname">%s</span><span class="hstat">%s</span></figcaption>`, esc(title), stat)
	fmt.Fprintf(b, `<div class="plot">%s<div class="tip" hidden></div></div></figure>`, svg)
}

// barCard renders a ranked list. Bars are all one colour on purpose: length
// already encodes the value, so a second encoding would only invite reading
// meaning into hues that carry none.
// showShare controls the "· 42%" suffix. It is right for a breakdown of one
// total (platforms, countries) and wrong for independent quantities side by side,
// where a share of their sum is a number that means nothing.
func barCard(b *strings.Builder, title, note string, items []labelCount, wide, showShare bool) {
	fmt.Fprintf(b, `<figure class="card"><figcaption><span class="epname">%s</span>`, esc(title))
	if note != "" {
		fmt.Fprintf(b, `<span class="hstat">%s</span>`, esc(note))
	}
	b.WriteString(`</figcaption>`)

	var total, max uint64
	for _, it := range items {
		total += it.N
		if it.N > max {
			max = it.N
		}
	}
	if len(items) == 0 || total == 0 {
		b.WriteString(`<div class="empty">no data in this window</div></figure>`)
		return
	}

	labClass := "blab"
	if wide {
		labClass = "blab wide"
	}
	for _, it := range items {
		width := float64(it.N) / float64(max) * 100
		// Only the count is emphasised; the share stays in secondary ink, outside
		// the <b>, so the eye lands on the number rather than on both equally.
		value := "<b>" + fmtCount(it.N) + "</b>"
		if showShare {
			value += " &middot; " + pct(it.N, total)
		}
		fmt.Fprintf(b, `<div class="brow"><span class="%s" title="%s">%s</span>`+
			`<span class="btrack"><span class="bfill" style="width:%.1f%%"></span></span>`+
			`<span class="bval">%s</span></div>`,
			labClass, esc(it.Label), esc(it.Label), width, value)
	}
	b.WriteString(`</figure>`)
}

func kpi(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, `<div class="kpi"><div class="k">%s</div><div class="v">%s</div></div>`, label, value)
}

func viewSwitch(views []int, current int) string {
	if len(views) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<nav class="views">`)
	for _, v := range views {
		label := fmt.Sprintf("%dd", v)
		if v == current {
			fmt.Fprintf(&b, `<span class="on" aria-current="page">%s</span>`, label)
			continue
		}
		fmt.Fprintf(&b, `<a href="?days=%d">%s</a>`, v, label)
	}
	b.WriteString(`</nav>`)
	return b.String()
}

func fmtCount(n uint64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64)) + "M"
	case n >= 10_000:
		return trimZero(strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64)) + "k"
	default:
		return strconv.FormatUint(n, 10)
	}
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func pct(part, whole uint64) string {
	if whole == 0 {
		return "0%"
	}
	return trimZero(strconv.FormatFloat(float64(part)/float64(whole)*100, 'f', 1, 64)) + "%"
}

// renderAdoption answers "does a new release get picked up as fast, and by as
// many people, as the last one" - which a per-release total cannot, because the
// counter only runs while a tag is `latest`. A five-month-old release therefore
// has five months of counting behind it and a fresh one has weeks, so the totals
// compare tenure, not popularity. Aligning both on days-since-release removes
// that.
func renderAdoption(b *strings.Builder, s *releaseStats) {
	if len(s.Curves) == 0 {
		b.WriteString(`<figure class="card"><figcaption><span class="epname">Release adoption` +
			`</span></figcaption><div class="empty">needs at least one release sampled over ` +
			`more than a day</div></figure>`)
		return
	}

	// Newest first: the line the reader came for is the top legend entry.
	curves := make([]releaseCurve, len(s.Curves))
	for i, c := range s.Curves {
		curves[len(s.Curves)-1-i] = c
	}

	var legend strings.Builder
	legend.WriteString(`<div class="legend">`)
	cols := make([][]float64, len(curves))
	labels := make([]string, len(curves))
	varNames := make([]string, len(curves))
	for k, c := range curves {
		fmt.Fprintf(&legend, `<span><span class="sw" style="background:var(%s)"></span>%s</span>`,
			curveVars[k%len(curveVars)], esc(c.Tag))
		labels[k] = c.Tag
		varNames[k] = curveVars[k%len(curveVars)]
		// Padded to the shared axis with the last value seen. The counter really
		// did stop moving there - because polling stopped, which the caption says.
		col := make([]float64, s.CurveDays)
		for i := range col {
			if i < len(c.Series) {
				col[i] = c.Series[i]
			} else {
				col[i] = c.Series[len(c.Series)-1]
			}
		}
		cols[k] = col
	}
	legend.WriteString(`</div>`)

	days := make([]string, s.CurveDays)
	for i := range days {
		days[i] = fmt.Sprintf("day %d", i)
	}

	b.WriteString(legend.String())
	chartCard(b, "Cumulative downloads by release age",
		fmt.Sprintf("day 0 &ndash; %d after each release", curveHorizon),
		curvesPanel(curves, s.CurveDays,
			fmt.Sprintf("Cumulative downloads per release over the first %d days after each release", curveHorizon)),
		seriesJSON(days, cols...), jsonStrings(labels...), jsonStrings(varNames...))

	horizon := make([]labelCount, 0, len(curves))
	for _, c := range curves {
		label := c.Tag
		if !c.Reached {
			label = fmt.Sprintf("%s (only %dd so far)", c.Tag, len(c.Series)-1)
		}
		horizon = append(horizon, labelCount{label, c.AtHorizon})
	}
	barCard(b, fmt.Sprintf("Downloads in the first %d days", curveHorizon),
		adoptionVerdict(curves), horizon, true, false)
}

// adoptionVerdict states the comparison in words, at the oldest age both releases
// have actually reached. Comparing a 27-day-old release against another one's
// 30-day figure would flatter or punish it for nothing but the calendar.
func adoptionVerdict(curves []releaseCurve) string {
	if len(curves) < 2 {
		return "only one release has a curve"
	}
	newest, prev := curves[0], curves[1]
	age := min(len(newest.Series), len(prev.Series)) - 1
	if age < 1 {
		return ""
	}
	a, bv := newest.Series[age], prev.Series[age]
	if bv <= 0 {
		return fmt.Sprintf("compared at day %d", age)
	}
	return fmt.Sprintf("at day %d, %s is %.2gx %s", age, esc(newest.Tag), a/bv, esc(prev.Tag))
}

func renderStats(s *releaseStats) []byte {
	var b strings.Builder
	b.Grow(64 << 10)

	// Rendering runs on the collector goroutine, where a panic takes the whole
	// process down - and it would take a redeploy to notice. An empty axis is the
	// only input that could do it, so it gets handled rather than trusted.
	if len(s.Days) == 0 {
		return []byte("<!doctype html><title>releases.pwndbg.re stats</title><p>no data")
	}

	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<meta name="robots" content="noindex">`)
	b.WriteString(`<title>releases.pwndbg.re stats</title><style>` + statsCSS + `</style></head><body>`)
	b.WriteString(`<div class="viz-root"><div class="wrap">`)

	fmt.Fprintf(&b, `<div class="head"><h1>releases.pwndbg.re &mdash; last %d days</h1>%s</div>`,
		s.Window, viewSwitch(s.Views, s.Window))
	fmt.Fprintf(&b, `<p class="sub">%s &rarr; %s &middot; generated %s UTC in %s</p>`,
		s.Days[0], s.Days[len(s.Days)-1],
		s.GeneratedAt.UTC().Format("2006-01-02 15:04"), s.Took.Round(time.Millisecond))

	b.WriteString(`<div class="kpis">`)
	kpi(&b, "Redirects", fmtCount(s.Total))
	kpi(&b, "Unique clients", fmtCount(s.UniqIPs))
	kpi(&b, "Countries", fmtCount(s.UniqCountries))
	kpi(&b, "Downloads on GitHub", fmtCount(s.GhTotal))
	b.WriteString(`</div>`)

	// ---- our own traffic
	b.WriteString(`<h2 class="sec">Redirects issued</h2>`)
	b.WriteString(`<p class="sub">What this host was asked for. A redirect is not a download: ` +
		`the client still has to follow it to GitHub, and a resumed or retried transfer asks twice. ` +
		`People who go to the GitHub releases page directly never appear here at all.</p>`)
	b.WriteString(`<div class="legend">` +
		`<span><span class="sw" style="background:var(--band)"></span>redirects per day</span>` +
		`<span><span class="swl"></span>unique clients per day</span></div>`)

	redirects := toFloats(s.Redirects)
	ips := toFloats(s.IPsPerDay)
	chartCard(&b, "Redirects and unique clients per day",
		fmt.Sprintf("<b>%s</b> redirects &middot; <b>%s</b> clients", fmtCount(s.Total), fmtCount(s.UniqIPs)),
		seriesPanel(redirects, [][]float64{ips}, []string{"c-ip"},
			"Redirects issued per day, with unique clients per day overlaid"),
		seriesJSON(s.Days, redirects, ips),
		jsonStrings("redirects", "unique clients"),
		jsonStrings("--band", "--ident"))

	b.WriteString(`<h2 class="sec">Who comes through here</h2>`)
	b.WriteString(`<p class="sub">Things only this host can know: GitHub reports how often a file was ` +
		`downloaded, never by whom or from where. Homebrew is the reason the client split is worth ` +
		`having at all &mdash; its architecture is in the User-Agent, because the formula picks the ` +
		`bottle, so the filename would not tell us.</p>`)
	b.WriteString(`<div class="row3">`)
	barCard(&b, "Client", "unique IPs", s.Clients, false, true)
	barCard(&b, "Country", "CF-IPCountry", s.Countries, false, true)
	barCard(&b, "Version requested", "", s.Versions, false, true)
	b.WriteString(`</div>`)

	// ---- GitHub's own counters
	b.WriteString(`<h2 class="sec">GitHub download counters</h2>`)
	b.WriteString(`<p class="sub">A different measurement with a different denominator: these are ` +
		`downloads GitHub itself counted, from every source, not just clients that came through here. ` +
		`They are cumulative and sampled hourly &mdash; and only for whichever release is ` +
		`<code>latest</code> at the time, so an older tag freezes at the value it had when its ` +
		`successor shipped. That is also why releases are compared by <em>age</em> rather than by ` +
		`total: a tag that stayed current for five months was counted for five months, so its total ` +
		`measures tenure as much as popularity. Every curve therefore covers the same first ` +
		`30 days; a line that stops early is a release that is not that old yet.</p>`)

	renderAdoption(&b, s)

	tag := s.GhLatestTag
	if tag == "" {
		tag = "current release"
	}
	b.WriteString(`<h3 class="sub" style="margin-top:24px;font-weight:600;color:var(--text-primary)">` +
		`What people download &mdash; ` + esc(tag) + `</h3>`)
	b.WriteString(`<p class="sub">One release, so every artifact here has been counted over the same ` +
		`span. Summed across releases they would not be comparable, because a tag stops being polled ` +
		`when its successor ships. An <code>other</code> row that is not tiny means the release ` +
		`naming changed and <code>classifyAsset</code> needs a look.</p>`)
	b.WriteString(`<div class="row3">`)
	barCard(&b, "Platform", "", s.Platforms, false, true)
	barCard(&b, "Variant", "", s.Variants, false, true)
	barCard(&b, "Format", "", s.Formats, false, true)
	b.WriteString(`</div>`)
	barCard(&b, "Downloads per artifact", esc(tag), s.Assets, true, true)

	b.WriteString(`<div class="row2">`)
	gh := toFloats(s.GhDaily)
	stat := "no snapshots yet"
	if s.GhLatestTag != "" {
		stat = fmt.Sprintf("%s &middot; <b>%s</b> total", esc(s.GhLatestTag), fmtCount(s.GhLatestTotal))
	}
	chartCard(&b, "Downloads per day (current release)", stat,
		seriesPanel(gh, nil, nil, "Downloads per day, differenced from GitHub's cumulative counter"),
		seriesJSON(s.Days, gh), jsonStrings("downloads"), jsonStrings("--band"))
	b.WriteString(`</div>`)

	b.WriteString(`<p class="foot">The two sections count different populations. Redirects are only ` +
		`clients that came through this host &mdash; the install script and Homebrew &mdash; so ` +
		`anyone who opens the GitHub releases page and clicks a file is invisible here; that is why ` +
		`the artifact breakdown is taken from GitHub&rsquo;s counters rather than from our log. ` +
		`Per-day GitHub numbers are differences between consecutive hourly ` +
		`samples of a cumulative counter, so they inherit its gaps: the first sample after a release ` +
		`is published is dropped rather than attributed to a day, which undercounts exactly the hours ` +
		`a release is busiest, and a pause in polling lands several hours of downloads on whichever ` +
		`day the next sample falls. Decreases are discarded. Country comes from Cloudflare&rsquo;s ` +
		`<code>CF-IPCountry</code> header &mdash; if IP geolocation is off at the edge every row reads ` +
		`<code>unknown</code>. Client buckets count distinct IPs and therefore do not add up to the ` +
		`headline figure, since one address can match two buckets over a long window.</p>`)

	b.WriteString(`</div></div><script>` + statsJS + `</script></body></html>`)
	return []byte(b.String())
}
