package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Panel dimensions. The viewBox is fixed and preserveAspectRatio="none" stretches
// it to the tile width - which is why every stroke carries
// vector-effect:non-scaling-stroke, or it would be distorted along with the X axis.
const (
	panelW = 340
	panelH = 96
	padTop = 8
	padBot = 14
)

// resolutionBudgetMs is maxResolutionTimeout in milliseconds - derived from the
// constant rather than written as a number, so changing the budget moves the line
// on the chart instead of leaving an untruth there.
var resolutionBudgetMs = float64(maxResolutionTimeout.Milliseconds())

// Palette. Every pair passed the validator: separation under deuteranopia and
// protanopia, plus contrast against the surface, checked separately for light and
// dark mode. The neutral is lighter in dark mode than in light (#c3c2b7 instead of
// #898781) because as a fourth peer series it collided with the yellow and the red.
const statsCSS = `
.viz-root{color-scheme:light;
 --surface-1:#fcfcfb;--plane:#f9f9f7;--text-primary:#0b0b0b;--text-secondary:#52514e;
 --muted:#898781;--grid:#e1e0d9;--axis:#c3c2b7;--border:rgba(11,11,11,.10);
 --served:#2a78d6;--aborted:#eda100;--notfound:#898781;--err:#e34948;
 --band:#86b6ef;--errband:#ec8a8a;--ident:#1baf7a}
@media (prefers-color-scheme:dark){:root:where(:not([data-theme="light"])) .viz-root{color-scheme:dark;
 --surface-1:#1a1a19;--plane:#0d0d0d;--text-primary:#fff;--text-secondary:#c3c2b7;
 --muted:#898781;--grid:#2c2c2a;--axis:#383835;--border:rgba(255,255,255,.10);
 --served:#3987e5;--aborted:#c98500;--notfound:#c3c2b7;--err:#d03b3b;
 --band:#256abf;--errband:#9c3232;--ident:#199e70}}
:root[data-theme="dark"] .viz-root{color-scheme:dark;
 --surface-1:#1a1a19;--plane:#0d0d0d;--text-primary:#fff;--text-secondary:#c3c2b7;
 --muted:#898781;--grid:#2c2c2a;--axis:#383835;--border:rgba(255,255,255,.10);
 --served:#3987e5;--aborted:#c98500;--notfound:#c3c2b7;--err:#d03b3b;
 --band:#256abf;--errband:#9c3232;--ident:#199e70}
*{box-sizing:border-box}
body{margin:0;font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
.viz-root{background:var(--plane);color:var(--text-primary);min-height:100vh;padding:28px 20px 56px}
.wrap{max-width:1160px;margin:0 auto}
h1{font-size:20px;margin:0 0 4px;letter-spacing:-.01em}
h2.sec{font-size:15px;margin:30px 0 12px;letter-spacing:-.01em}
.sub{color:var(--text-secondary);margin:0 0 20px;font-size:13px}
.sub code{font-size:12px}
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
.legend{display:flex;flex-wrap:wrap;gap:14px;align-items:center;margin:0 0 14px;font-size:12.5px;color:var(--text-secondary)}
.sw{display:inline-block;width:11px;height:11px;border-radius:3px;margin-right:6px;vertical-align:-1px}
.hostrow{margin:0 0 20px}
.rowhead{display:flex;flex-wrap:wrap;align-items:baseline;gap:8px;margin:0 2px 7px}
.row3{display:grid;grid-template-columns:repeat(3,1fr);gap:12px}
@media (max-width:820px){.row3{grid-template-columns:1fr}}
.card{margin:0;background:var(--surface-1);border:1px solid var(--border);border-radius:10px;padding:10px 11px 9px}
figcaption{display:flex;flex-wrap:wrap;align-items:baseline;gap:6px;margin-bottom:4px}
.epname{font-size:12px;font-weight:600;color:var(--text-primary)}
.hname{font-weight:600;font-size:13.5px}
.hstat{margin-left:auto;color:var(--text-secondary);font-size:12px}
.hstat b{color:var(--text-primary);font-weight:600}
.tag{font-size:11px;color:var(--muted);border:1px solid var(--border);border-radius:20px;padding:1px 8px}
.plot{position:relative}
.spark{display:block;width:100%;height:96px}
.a{stroke:var(--surface-1);stroke-width:.6;vector-effect:non-scaling-stroke}
.s0{fill:var(--served)}.s1{fill:var(--aborted)}.s2{fill:var(--notfound)}.s3{fill:var(--err)}
.band{fill:var(--band)}
.card.fail .band{fill:var(--errband)}
.card.fail .ln50{stroke:var(--err)}
.ln50{fill:none;stroke:var(--served);stroke-width:1.6;vector-effect:non-scaling-stroke}
.ln{fill:none;stroke-width:1.7;vector-effect:non-scaling-stroke}
.c-ip{stroke:var(--served)}.c-bid{stroke:var(--ident)}
.axis{stroke:var(--axis);stroke-width:1;vector-effect:non-scaling-stroke}
.thr{stroke:var(--muted);stroke-width:1;stroke-dasharray:3 3;vector-effect:non-scaling-stroke}
.swline{display:inline-block;width:14px;height:0;border-top:1px dashed var(--muted);margin-right:6px;vertical-align:3px}
.cross{stroke:var(--text-secondary);stroke-width:1;vector-effect:non-scaling-stroke;opacity:.6}
.brow{display:flex;align-items:center;gap:10px;padding:3px 0}
.blab{flex:0 0 128px;font-size:12.5px;color:var(--text-secondary)}
.btrack{flex:1;height:14px;background:var(--grid);border-radius:4px;overflow:hidden}
.bfill{display:block;height:100%;background:var(--served);border-radius:4px}
.bval{flex:0 0 78px;text-align:right;font-size:12.5px;font-variant-numeric:tabular-nums}
.bsub{flex:0 0 92px;text-align:right;font-size:12px;color:var(--text-muted);font-variant-numeric:tabular-nums}
.btail{padding:7px 0 1px;font-size:12px;color:var(--text-muted);border-top:1px solid var(--grid);margin-top:5px}
.off{margin-left:6px;padding:0 5px;border-radius:9px;font-size:10.5px;font-weight:600;letter-spacing:.02em;
     color:var(--err);border:1px solid var(--err);vertical-align:1px;white-space:nowrap}
.tip{position:absolute;pointer-events:none;background:var(--surface-1);border:1px solid var(--border);
 border-radius:8px;padding:6px 9px;font-size:12px;box-shadow:0 4px 14px rgba(0,0,0,.14);
 white-space:nowrap;z-index:5;font-variant-numeric:tabular-nums;line-height:1.45}
.tip i{display:inline-block;width:8px;height:8px;border-radius:2px;margin-right:5px}
.foot{color:var(--muted);font-size:11.5px;margin-top:20px;max-width:74ch}
`

const statsJS = `
document.querySelectorAll('.card[data-series]').forEach(card=>{
 const data=JSON.parse(card.dataset.series);
 const names=JSON.parse(card.dataset.labels), vars=JSON.parse(card.dataset.vars);
 const svg=card.querySelector('svg'), tip=card.querySelector('.tip');
 const cross=card.querySelector('.cross'), plot=card.querySelector('.plot');
 const vbW=svg.viewBox.baseVal.width;
 plot.addEventListener('pointermove',e=>{
  const r=plot.getBoundingClientRect();
  const f=Math.min(1,Math.max(0,(e.clientX-r.left)/r.width));
  const i=Math.round(f*(data.length-1)), row=data[i];
  let h='<b>'+row[0]+'</b>';
  for(let k=0;k<names.length;k++)
   h+='<br><i style="background:var('+vars[k]+')"></i>'+names[k]+' '+row[k+1].toLocaleString('en');
  tip.innerHTML=h; tip.hidden=false;
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

func svgHead(b *strings.Builder, aria string) {
	fmt.Fprintf(b, `<svg class="spark" viewBox="0 0 %d %d" preserveAspectRatio="none" role="img" aria-label="%s">`,
		panelW, panelH, esc(aria))
}

func svgTail(b *strings.Builder, g svgGeom) {
	fmt.Fprintf(b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
		g.x(0), g.base(), g.x(g.n-1), g.base())
	fmt.Fprintf(b, `<line class="cross" x1="0" y1="%d" x2="0" y2="%.1f" style="display:none"/></svg>`,
		padTop, g.base())
}

func polygon(b *strings.Builder, g svgGeom, vals []float64, max float64, class string) {
	if !anyPositive(vals) {
		return
	}
	b.WriteString(`<polygon class="` + class + `" points="`)
	fmt.Fprintf(b, "%.1f,%.1f ", g.x(0), g.base())
	for i, v := range vals {
		fmt.Fprintf(b, "%.1f,%.1f ", g.x(i), g.y(v, max))
	}
	fmt.Fprintf(b, `%.1f,%.1f"/>`, g.x(g.n-1), g.base())
}

func polyline(b *strings.Builder, g svgGeom, vals []float64, max float64, class string) {
	if len(vals) == 0 {
		return
	}
	fmt.Fprintf(b, `<path class="%s" d="M%.1f,%.1f`, class, g.x(0), g.y(vals[0], max))
	for i, v := range vals {
		fmt.Fprintf(b, "L%.1f,%.1f", g.x(i), g.y(v, max))
	}
	b.WriteString(`"/>`)
}

func anyPositive(v []float64) bool {
	for _, x := range v {
		if x > 0 {
			return true
		}
	}
	return false
}

// trafficPanel draws four stacked layers plus per-day ticks for the two rare
// categories. Aborted and 5xx are a fraction of a per mille of traffic - as a stack
// segment they would be thinner than a pixel, so they also get ticks below the axis.
func trafficPanel(series []statusCounts, max float64, aria string, win int) string {
	g := svgGeom{n: len(series)}
	raw := make([][2]float64, len(series))
	cum := make([][4]float64, len(series))
	for i, c := range series {
		raw[i] = [2]float64{float64(c.Aborted), float64(c.Err5xx)}
		cum[i] = [4]float64{
			float64(c.Served),
			float64(c.Served + c.Aborted),
			float64(c.Served + c.Aborted + c.NotFound),
			float64(c.total()),
		}
	}
	sm := smoothQuad(cum, win)

	var b strings.Builder
	svgHead(&b, aria)
	for k := 3; k >= 0; k-- {
		layer := make([]float64, len(sm))
		for i := range sm {
			layer[i] = sm[i][k]
		}
		polygon(&b, g, layer, max, fmt.Sprintf("a s%d", k))
	}
	rare := false
	for _, r := range raw {
		if r[0] > 0 || r[1] > 0 {
			rare = true
			break
		}
	}
	if rare {
		y1 := g.base() + 3
		for i, r := range raw {
			if r[0] > 0 {
				fmt.Fprintf(&b, `<rect class="s1" x="%.1f" y="%.1f" width="1.8" height="2.6"/>`, g.x(i)-0.9, y1)
			}
			if r[1] > 0 {
				fmt.Fprintf(&b, `<rect class="s3" x="%.1f" y="%.1f" width="1.8" height="2.6"/>`, g.x(i)-0.9, y1+3.6)
			}
		}
	}
	svgTail(&b, g)
	return b.String()
}

func smoothQuad(vals [][4]float64, win int) [][4]float64 {
	if win < 1 {
		win = 1
	}
	out := make([][4]float64, len(vals))
	for i := range vals {
		lo := i - win + 1
		if lo < 0 {
			lo = 0
		}
		var acc [4]float64
		for _, v := range vals[lo : i+1] {
			for k := 0; k < 4; k++ {
				acc[k] += v[k]
			}
		}
		cnt := float64(i - lo + 1)
		for k := 0; k < 4; k++ {
			out[i][k] = acc[k] / cnt
		}
	}
	return out
}

// bandPanel: a p95 band with the p50 line inside it. With no samples at all we
// draw nothing - a band lying on the axis would read as a measured zero, when it is
// really an absence of data.
//
// threshold > 0 adds a horizontal reference line (e.g. the resolution time budget).
// It is drawn only when it fits within the panel's scale: for a host that never came
// close to the threshold, stretching the axis up to it would flatten the whole curve
// to zero and take away the only information the panel carries. The threshold's
// label goes in the legend, not in the SVG - preserveAspectRatio is "none", so text
// inside would be distorted along with the X axis.
func bandPanel(pairs [][2]float64, max float64, aria string, threshold float64, win int) string {
	g := svgGeom{n: len(pairs)}
	sm := smoothPairs(pairs, win)
	hi := make([]float64, len(sm))
	lo := make([]float64, len(sm))
	for i, v := range sm {
		lo[i], hi[i] = v[0], v[1]
	}
	var b strings.Builder
	svgHead(&b, aria)
	if anyPositive(hi) {
		polygon(&b, g, hi, max, "band")
		polyline(&b, g, lo, max, "ln50")
		if threshold > 0 && threshold <= max {
			y := g.y(threshold, max)
			fmt.Fprintf(&b, `<line class="thr" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
				g.x(0), y, g.x(g.n-1), y)
		}
	}
	svgTail(&b, g)
	return b.String()
}

func linesPanel(sets [][]float64, classes []string, aria string, threshold float64, win int) (string, float64) {
	g := svgGeom{n: len(sets[0])}
	var max float64
	smoothed := make([][]float64, len(sets))
	for i, s := range sets {
		smoothed[i] = smoothOne(s, win)
		for _, v := range smoothed[i] {
			if v > max {
				max = v
			}
		}
	}
	if max == 0 {
		max = 1
	}
	// The threshold may sit above the data - then the scale is stretched to reach it,
	// because for a space budget the meaningful question is "how much have we eaten",
	// unlike with latency, where the shape of the curve matters more than the
	// reference.
	if threshold > 0 && threshold > max {
		max = threshold
	}
	var b strings.Builder
	svgHead(&b, aria)
	for i, s := range smoothed {
		polyline(&b, g, s, max, "ln "+classes[i])
	}
	if threshold > 0 && threshold <= max {
		y := g.y(threshold, max)
		fmt.Fprintf(&b, `<line class="thr" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`,
			g.x(0), y, g.x(g.n-1), y)
	}
	svgTail(&b, g)
	return b.String(), max
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
			b.WriteString(fmtNum(c[i]))
		}
		b.WriteByte(']')
	}
	b.WriteByte(']')
	return b.String()
}

// fmtNum rounds to two decimals and prints the shortest form. The counters are
// integers, so without this each of the ~7000 values on the page would carry a
// pointless ".00" - across this many panels that is tens of kilobytes for nothing.
func fmtNum(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

func jsonStrings(items ...string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = `"` + s + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func card(b *strings.Builder, title, stat, svg, data, labels, vars, extraClass string) {
	fmt.Fprintf(b, `<figure class="card%s" data-series='%s' data-labels='%s' data-vars='%s'>`,
		extraClass, data, labels, vars)
	fmt.Fprintf(b, `<figcaption><span class="epname">%s</span><span class="hstat">%s</span></figcaption>`, title, stat)
	fmt.Fprintf(b, `<div class="plot">%s<div class="tip" hidden></div></div></figure>`, svg)
}

func renderStats(s *statsSnapshot) []byte {
	var b strings.Builder
	b.Grow(512 << 10)

	var grand statusCounts
	for _, byEp := range s.Traffic {
		for _, series := range byEp {
			for _, c := range series {
				grand.Served += c.Served
				grand.Aborted += c.Aborted
				grand.NotFound += c.NotFound
				grand.Err5xx += c.Err5xx
			}
		}
	}

	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<meta name="robots" content="noindex">`)
	b.WriteString(`<title>debuginfod.pwndbg.re stats</title><style>` + statsCSS + `</style></head><body>`)
	b.WriteString(`<div class="viz-root"><div class="wrap">`)

	cur := len(s.Days) - 1
	fmt.Fprintf(&b, `<div class="head"><h1>debuginfod.pwndbg.re &mdash; last %d days</h1>%s</div>`,
		cur, viewSwitch(s.Views, cur))
	smoothNote := "raw daily values"
	if s.Smooth > 1 {
		smoothNote = fmt.Sprintf("%d-day moving average", s.Smooth)
	}
	fmt.Fprintf(&b, `<p class="sub">%s &rarr; %s &middot; %s &middot; generated %s UTC in %s</p>`,
		s.Days[0], s.Days[len(s.Days)-1], smoothNote,
		s.GeneratedAt.UTC().Format("2006-01-02 15:04"), s.Took.Round(time.Millisecond))

	b.WriteString(`<div class="kpis">`)
	kpi(&b, "Requests", fmtCount(grand.total()))
	kpi(&b, "Served complete", fmtCount(grand.Served))
	kpi(&b, "Client aborted", fmtCount(grand.Aborted))
	kpi(&b, "Server errors 5xx", fmtCount(grand.Err5xx))
	b.WriteString(`</div>`)

	b.WriteString(`<div class="legend">` +
		swatch("--served", "served &mdash; 200, complete") +
		swatch("--aborted", "aborted &mdash; 200, client disconnected") +
		swatch("--notfound", "not found &mdash; 404 / 501 / 503") +
		swatch("--err", "5xx &mdash; server error") + `</div>`)
	smoothing := "Areas are raw daily values."
	if s.Smooth > 1 {
		smoothing = fmt.Sprintf("Areas are %d-day moving averages.", s.Smooth)
	}
	b.WriteString(`<p class="sub">` + smoothing + ` The three panels in a row share one Y scale, ` +
		`so endpoints are comparable within a host; scales differ between hosts. Aborted and 5xx are a fraction ` +
		`of a percent of traffic, so they are also drawn as per-day ticks below the axis &mdash; a stack segment ` +
		`that size cannot be rendered.</p>`)

	renderClients(&b, s)
	renderTraffic(&b, s)
	renderBytes(&b, s)
	renderProbes(&b, s)
	renderThroughput(&b, s)
	renderCountries(&b, s)
	renderCache(&b, s)

	b.WriteString(`<p class="foot">Y scale is shared across a host&rsquo;s three panels but independent between ` +
		`hosts, because host volumes span several orders of magnitude &mdash; compare shape between rows, not ` +
		`height. &ldquo;Aborted&rdquo; is a response that started with 200 and then failed mid-transfer, so it ` +
		`counts as a failure even though the status line says success. &ldquo;Not found&rdquo; also covers 503: ` +
		`that status comes from an upstream this proxy has deliberately marked down, so it is a resolution ` +
		`outcome rather than a server fault, and only genuine 5xx are counted as errors. Byte counts are bytes ` +
		`<em>as sent</em>, i.e. compressed.</p>`)

	b.WriteString(`</div></div><script>` + statsJS + `</script></body></html>`)
	return []byte(b.String())
}

// viewSwitch draws the window-length switcher. These are plain links, not
// JavaScript: every view is its own finished page on the server, so the address can
// be bookmarked and shared, and switching works with scripting disabled.
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

func kpi(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, `<div class="kpi"><div class="k">%s</div><div class="v">%s</div></div>`, label, value)
}

func swatch(cssVar, label string) string {
	return fmt.Sprintf(`<span><span class="sw" style="background:var(%s)"></span>%s</span>`, cssVar, label)
}

func renderClients(b *strings.Builder, s *statsSnapshot) {
	ips := toFloats(s.Clients)
	bids := toFloats(s.BuildIDs)
	req := toFloats(s.Requests)
	rpc := ratio(req, ips)
	rpb := ratio(req, bids)

	b.WriteString(`<h2 class="sec">Who is making these requests</h2>`)
	b.WriteString(`<p class="sub">Unique clients (by <code>remote_ip</code> behind Cloudflare) and unique build IDs ` +
		`per day, so the counts below read as load from many clients rather than one busy client.</p>`)
	b.WriteString(`<div class="legend">` + swatch("--served", "clients") + swatch("--ident", "build IDs") + `</div>`)
	b.WriteString(`<div class="row3" style="margin-bottom:18px">`)

	svg1, _ := linesPanel([][]float64{ips, bids}, []string{"c-ip", "c-bid"}, "Unique clients and build IDs per day", 0, s.Smooth)
	card(b, "unique per day",
		fmt.Sprintf("median <b>%.0f</b> clients &middot; <b>%.0f</b> build IDs", median(ips), median(bids)),
		svg1, seriesJSON(s.Days, ips, bids), jsonStrings("clients", "build IDs"),
		jsonStrings("--served", "--ident"), "")

	svg2, _ := linesPanel([][]float64{rpc}, []string{"c-ip"}, "Requests per client per day", 0, s.Smooth)
	card(b, "requests per client",
		fmt.Sprintf("median <b>%.0f</b> &middot; peak %.0f", median(rpc), maxOf(rpc)),
		svg2, seriesJSON(s.Days, rpc), jsonStrings("requests / client"), jsonStrings("--served"), "")

	svg3, _ := linesPanel([][]float64{rpb}, []string{"c-bid"}, "Requests per build ID per day", 0, s.Smooth)
	card(b, "requests per build ID",
		fmt.Sprintf("median <b>%.0f</b> &middot; peak %.0f", median(rpb), maxOf(rpb)),
		svg3, seriesJSON(s.Days, rpb), jsonStrings("requests / build ID"), jsonStrings("--ident"), "")

	b.WriteString(`</div>`)
}

// offlineBadge is one piece of markup shared by both panels that name hosts.
//
// It is a function because it is used twice and the first version marked hosts
// in the probe panel only, so the same upstream read as live in one section and
// dead in the other. It is also not colour alone: the word "offline" and the
// title carry the meaning for a colourblind reader, a greyscale print and a
// screen reader alike.
func offlineBadge(off bool) string {
	if !off {
		return ""
	}
	return `<span class="off" title="no probe in the last 24 h - this upstream is no longer ` +
		`in the servers map">offline</span>`
}

func renderTraffic(b *strings.Builder, s *statsSnapshot) {
	b.WriteString(`<h2 class="sec">Traffic by upstream and endpoint</h2>`)
	for _, host := range s.Hosts {
		byEp := s.Traffic[host]

		// One scale for the whole row: a host's endpoints must be comparable with
		// each other, otherwise every panel lies about the proportions.
		var max float64
		var tot statusCounts
		for _, ep := range statsEndpoints {
			series := byEp[ep]
			if series == nil {
				continue
			}
			cum := make([][4]float64, len(series))
			for i, c := range series {
				cum[i] = [4]float64{0, 0, 0, float64(c.total())}
				tot.Served += c.Served
				tot.Aborted += c.Aborted
				tot.NotFound += c.NotFound
				tot.Err5xx += c.Err5xx
			}
			for _, v := range smoothQuad(cum, s.Smooth) {
				if v[3] > max {
					max = v[3]
				}
			}
		}
		if max == 0 {
			max = 1
		}

		// unresolved and offline are mutually exclusive - the first is the
		// absence of a host, and offlineHosts skips it for exactly that reason.
		tag := offlineBadge(s.isOffline(host))
		if host == unresolvedLabel {
			tag = `<span class="tag">never reached an upstream</span>`
		}
		fmt.Fprintf(b, `<section class="hostrow"><div class="rowhead"><span class="hname">%s</span>%s`+
			`<span class="hstat"><b>%s</b> requests &middot; <b>%s</b> served &middot; shared Y, peak %s/day`+
			`</span></div><div class="row3">`,
			esc(host), tag, fmtCount(tot.total()), pct(tot.Served, tot.total()), fmtCount(uint64(max)))

		for _, ep := range statsEndpoints {
			series := byEp[ep]
			if series == nil {
				series = make([]statusCounts, len(s.Days))
			}
			var epTot statusCounts
			aborted := make([]float64, len(series))
			err5 := make([]float64, len(series))
			served := make([]float64, len(series))
			notfound := make([]float64, len(series))
			for i, c := range series {
				epTot.Served += c.Served
				epTot.Aborted += c.Aborted
				epTot.NotFound += c.NotFound
				epTot.Err5xx += c.Err5xx
				served[i], aborted[i] = float64(c.Served), float64(c.Aborted)
				notfound[i], err5[i] = float64(c.NotFound), float64(c.Err5xx)
			}
			stat := "no traffic"
			if epTot.total() > 0 {
				stat = fmt.Sprintf("%s &middot; %s served", fmtCount(epTot.total()), pct(epTot.Served, epTot.total()))
			}
			card(b, ep, stat,
				trafficPanel(series, max, "Daily "+ep+" traffic for "+host, s.Smooth),
				seriesJSON(s.Days, served, aborted, notfound, err5),
				jsonStrings("served", "aborted", "not found", "5xx"),
				jsonStrings("--served", "--aborted", "--notfound", "--err"), "")
		}
		b.WriteString(`</div></section>`)
	}
}

func renderBytes(b *strings.Builder, s *statsSnapshot) {
	b.WriteString(`<h2 class="sec">Bytes served</h2><div class="kpis">`)
	for i, label := range []string{"Last 24 h", "Last 7 days", "Last 30 days", "Full range"} {
		kpi(b, label, fmtBytes(s.BytesTotal[i]))
	}
	b.WriteString(`</div>`)

	type row struct {
		host string
		val  uint64
	}
	rows := make([]row, 0, len(s.Bytes))
	var max uint64
	for h, v := range s.Bytes {
		if v[3] == 0 {
			continue
		}
		rows = append(rows, row{h, v[3]})
		if v[3] > max {
			max = v[3]
		}
	}
	if len(rows) == 0 {
		return
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].val > rows[j-1].val; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}

	fmt.Fprintf(b, `<div class="card"><figcaption><span class="epname">By upstream</span>`+
		`<span class="hstat">total <b>%s</b></span></figcaption>`, fmtBytes(s.BytesTotal[3]))
	for _, r := range rows {
		fmt.Fprintf(b, `<div class="brow"><span class="blab">%s%s</span>`+
			`<span class="btrack"><span class="bfill" style="width:%.2f%%"></span></span>`+
			`<span class="bval">%s</span></div>`,
			esc(r.host), offlineBadge(s.isOffline(r.host)),
			100*float64(r.val)/float64(max), fmtBytes(r.val))
	}
	b.WriteString(`</div>`)
}

// offlineNote explains the badge where it appears, and says nothing when it
// does not - including on a day quiet enough that nothing was probed, where the
// absence of evidence must not read as evidence of absence.
func offlineNote(s *statsSnapshot) string {
	n := s.offlineCount()
	if n == 0 {
		return ""
	}
	verb, pronoun := "is", "it is"
	if n > 1 {
		verb, pronoun = "are", "they are"
	}
	return fmt.Sprintf(` <b>%d</b> of these went unprobed in the last 24 h and %s marked `+
		`<span class="off">offline</span> &mdash; %s charted because there is history, `+
		`not because anything is still asking.`, n, verb, pronoun)
}

func renderProbes(b *strings.Builder, s *statsSnapshot) {
	b.WriteString(`<h2 class="sec">Resolution probes &mdash; per debuginfod backend</h2>`)
	fmt.Fprintf(b, `<p class="sub">From <code>resolve_logs</code>: every cold build ID is probed at <em>all</em> `+
		`upstreams concurrently, so each backend sees roughly the same probe count. Latency is measured separately `+
		`on probes that answered and on those that did not. %s probes &middot; %s resolved (%s).%s</p>`,
		fmtCount(s.ProbeTotal), fmtCount(s.ProbeOK), pct(s.ProbeOK, s.ProbeTotal), offlineNote(s))
	b.WriteString(`<div class="legend">` + swatch("--served", "resolved / p50") +
		swatch("--notfound", "no answer") + swatch("--band", "p95 band, successful") +
		swatch("--err", "failed probe latency") +
		fmt.Sprintf(`<span><span class="swline"></span>%s resolution budget</span>`,
			maxResolutionTimeout) + `</div>`)

	for _, host := range s.ProbeHosts {
		series := s.Probes[host]
		var ok, fail uint64
		stack := make([][4]float64, len(series))
		okPairs := make([][2]float64, len(series))
		failPairs := make([][2]float64, len(series))
		for i, p := range series {
			ok += p.OK
			fail += p.Fail
			stack[i] = [4]float64{float64(p.OK), 0, 0, float64(p.OK + p.Fail)}
			okPairs[i] = [2]float64{float64(p.P50), float64(p.P95)}
			failPairs[i] = [2]float64{float64(p.FailP50), float64(p.FailP95)}
		}
		sm := smoothQuad(stack, s.Smooth)
		var pmax float64
		for _, v := range sm {
			if v[3] > pmax {
				pmax = v[3]
			}
		}
		if pmax == 0 {
			pmax = 1
		}

		g := svgGeom{n: len(series)}
		var pb strings.Builder
		svgHead(&pb, "Daily resolution probes for "+host)
		totLayer := make([]float64, len(sm))
		okLayer := make([]float64, len(sm))
		for i, v := range sm {
			totLayer[i], okLayer[i] = v[3], v[0]
		}
		polygon(&pb, g, totLayer, pmax, "a s2")
		polygon(&pb, g, okLayer, pmax, "a s0")
		svgTail(&pb, g)

		okMax := maxPairHi(smoothPairs(okPairs, s.Smooth))
		failMax := maxPairHi(smoothPairs(failPairs, s.Smooth))

		fmt.Fprintf(b, `<section class="hostrow"><div class="rowhead"><span class="hname">%s</span>%s`+
			`<span class="hstat"><b>%s</b> probes &middot; <b>%s</b> resolved (%s)</span></div><div class="row3">`,
			esc(host), offlineBadge(s.isOffline(host)), fmtCount(ok+fail), fmtCount(ok), pct(ok, ok+fail))

		okF, failF := toFloats(colOK(series)), toFloats(colFail(series))
		card(b, "probes", fmt.Sprintf("peak %s/day", fmtCount(uint64(pmax))),
			pb.String(), seriesJSON(s.Days, okF, failF),
			jsonStrings("resolved", "no answer"), jsonStrings("--served", "--notfound"), "")

		slab := "no successful probes"
		if ok > 0 {
			slab = fmt.Sprintf("p95 peak %.0f ms", okMax)
		}
		card(b, "latency, successful", slab,
			bandPanel(okPairs, okMax, "Successful probe latency for "+host, resolutionBudgetMs, s.Smooth),
			seriesJSON(s.Days, pairCol(okPairs, 0), pairCol(okPairs, 1)),
			jsonStrings("p50 ms", "p95 ms"), jsonStrings("--served", "--band"), "")

		flab := "no failed probes"
		if fail > 0 {
			flab = fmt.Sprintf("p95 peak %.0f ms", failMax)
		}
		card(b, "latency, failed", flab,
			bandPanel(failPairs, failMax, "Failed probe latency for "+host, resolutionBudgetMs, s.Smooth),
			seriesJSON(s.Days, pairCol(failPairs, 0), pairCol(failPairs, 1)),
			jsonStrings("p50 ms", "p95 ms"), jsonStrings("--err", "--errband"), " fail")

		b.WriteString(`</div></section>`)
	}
}

func renderThroughput(b *strings.Builder, s *statsSnapshot) {
	if len(s.ThruHosts) == 0 {
		return
	}
	b.WriteString(`<h2 class="sec">Upstream throughput &mdash; cache misses only</h2>`)
	fmt.Fprintf(b, `<p class="sub">Bytes delivered per second over responses above 100&nbsp;KiB that were `+
		`<em>not</em> served from cache. Throughput rather than raw duration, because <code>duration_ms</code> `+
		`grows with file size and cannot be compared between hosts. <b>All panels share one Y scale</b>, capped `+
		`at %.1f MiB/s.</p>`, s.ThruMax)
	b.WriteString(`<div class="legend">` + swatch("--served", "p50 MiB/s") + swatch("--band", "p90 band") + `</div>`)
	b.WriteString(`<div class="row3">`)
	for _, host := range s.ThruHosts {
		series := s.Thru[host]
		pairs := thruPairs(series)
		var n uint64
		p50 := make([]float64, len(series))
		p90 := make([]float64, len(series))
		for i, t := range series {
			n += t.N
			p50[i], p90[i] = t.P50, t.P90
		}
		card(b, esc(host)+offlineBadge(s.isOffline(host)),
			fmt.Sprintf("%s transfers &middot; median <b>%.2f</b> MiB/s", fmtCount(n), median(p50)),
			bandPanel(pairs, s.ThruMax, "Upstream throughput for "+host, 0, s.Smooth),
			seriesJSON(s.Days, p50, p90),
			jsonStrings("p50 MiB/s", "p90 MiB/s"), jsonStrings("--served", "--band"), "")
	}
	b.WriteString(`</div>`)
}

// countryRows is how many countries get their own bar before the rest are
// folded into one. Past a dozen the bars are too short to compare and the list
// stops being a ranking and becomes a table nobody reads.
const countryRows = 12

// flagFor turns an ISO 3166-1 alpha-2 code into its flag emoji, which is just
// the two letters shifted into the regional-indicator block.
//
// It returns "" for anything that is not exactly two ASCII letters. That guard
// is not decoration: country originates in the CF-IPCountry header, which any
// process on this host can set (loopback is trusted), so the value reaching
// here is not guaranteed to be a country at all. Cloudflare also sends XX for
// an address it cannot place and T1 for Tor, neither of which is a country -
// they are shown as plain labels rather than as whatever glyph pair the
// arithmetic would produce.
func flagFor(code string) string {
	if len(code) != 2 || code == "XX" || code == "T1" {
		return ""
	}
	var r [2]rune
	for i := range 2 {
		c := code[i]
		if c < 'A' || c > 'Z' {
			return ""
		}
		r[i] = rune(c-'A') + 0x1F1E6
	}
	return string(r[:])
}

// countryLabel renders a country code for display: its flag where one can be
// derived, and the code itself always escaped.
//
// It is a function rather than two lines at each call site because there are
// two call sites - the bar row and the "Top country" headline - and the first
// version escaped only one of them. country comes from the CF-IPCountry header,
// so that was a live injection point on a page an operator opens;
// TestCountryPanelEscapesAttackerControlledLabels caught it and now pins it.
func countryLabel(code string) string {
	if flag := flagFor(code); flag != "" {
		return flag + " " + esc(code)
	}
	return esc(code)
}

// renderCountries draws where the traffic came from.
//
// A ranked bar list rather than a map or a time series: the question is which
// places dominate and by how much, and with ~200 categories and a very long
// tail that is a comparison of magnitudes, not a shape over time. It reuses the
// same .brow/.bfill row as the bytes panel instead of introducing a second
// visual language for the same job.
//
// Two numbers per row, because one of them alone misleads in a way this service
// actually sees: requests say how much came from a country, peak clients/day
// say whether that was many people or one machine in a loop. Measured on
// production, SK sent 60k requests from 10 addresses.
func renderCountries(b *strings.Builder, s *statsSnapshot) {
	if len(s.Countries) == 0 {
		// No rows rather than an empty frame: a country panel showing nothing
		// reads as "nobody is using this", when it means the column was never
		// filled. See scripts/backfill_country.py.
		return
	}

	var rows []countryRow
	var total uint64
	for _, c := range s.Countries {
		total += sumU(s.Country[c])
	}
	if total == 0 {
		return
	}

	var otherReq uint64
	otherN := 0
	for i, c := range s.Countries {
		req := sumU(s.Country[c])
		if req == 0 {
			continue
		}
		if i < countryRows {
			rows = append(rows, countryRow{label: c, requests: req, peak: maxU(s.CountryClients[c]), n: 1})
			continue
		}
		otherReq += req
		otherN++
	}
	if len(rows) == 0 {
		return
	}

	top := rows[0].requests

	b.WriteString(`<h2 class="sec">Where requests come from</h2><div class="kpis">`)
	kpi(b, "Countries", fmtCount(uint64(len(s.Countries))))
	kpi(b, "Top country", countryLabel(rows[0].label)+" "+pct(rows[0].requests, total))
	kpi(b, "Top 5 share", pct(sumTop(rows, 5), total))
	b.WriteString(`</div>`)

	fmt.Fprintf(b, `<div class="card"><figcaption><span class="epname">By country</span>`+
		`<span class="hstat">requests &middot; peak clients/day</span></figcaption>`)
	for _, r := range rows {
		fmt.Fprintf(b, `<div class="brow"><span class="blab">%s</span>`+
			`<span class="btrack"><span class="bfill" style="width:%.2f%%"></span></span>`+
			`<span class="bval">%s</span><span class="bsub">%s clients</span></div>`,
			countryLabel(r.label), 100*float64(r.requests)/float64(top),
			fmtCount(r.requests), fmtCount(r.peak))
	}

	// The tail gets a line, never a bar. It is the sum of dozens of countries,
	// so on a scale where the leading country is full width it is routinely
	// wider than the track - the first version drew it at 257% and the bar
	// silently clipped, making the residual look like the largest single
	// origin. It is not a peer of the rows above it and should not be drawn as
	// one.
	if otherN > 0 {
		fmt.Fprintf(b, `<div class="btail">+ %d more countries &middot; %s requests (%s)</div>`,
			otherN, fmtCount(otherReq), pct(otherReq, total))
	}
	b.WriteString(`</div>`)
}

// countryRow is one bar. n > 1 marks the folded tail.
type countryRow struct {
	label    string
	requests uint64
	peak     uint64
	n        int
}

// sumTop adds the first k rows, stopping at the folded tail: "top 5 share" has
// to mean five countries, and counting the "other" row towards it would fold
// the whole long tail into the headline it is meant to be contrasted with.
func sumTop(rows []countryRow, k int) uint64 {
	var sum uint64
	for i, r := range rows {
		if i >= k || r.n > 1 {
			break
		}
		sum += r.requests
	}
	return sum
}

func renderCache(b *strings.Builder, s *statsSnapshot) {
	// An empty cache_stats table means the measurement has only just started. A
	// section of flat zeros would suggest an empty cache, so it is better not to
	// show it at all.
	if !s.HasCacheStats {
		return
	}
	u := s.CacheLast
	b.WriteString(`<h2 class="sec">Cache and disk</h2><div class="kpis">`)
	kpi(b, "Cache on disk", fmtBytes(u.Bytes))
	kpi(b, "Blobs", fmtCount(u.Entries))
	if u.FsTotal > 0 {
		kpi(b, "Partition free", fmtBytes(u.FsFree))
		kpi(b, "Partition used", pct(u.FsTotal-u.FsFree, u.FsTotal))
	} else {
		kpi(b, "Abandoned .tmp", fmtBytes(u.TmpBytes))
	}
	b.WriteString(`</div>`)

	budget := ""
	if s.CacheMaxBytes > 0 {
		budget = fmt.Sprintf(` The dashed line is <code>CACHE_MAX_BYTES</code> (%s); eviction targets a `+
			`fraction below it, so the curve should ride under the line rather than touch it.`,
			fmtBytes(s.CacheMaxBytes))
	}
	// The gap between occupancy and length is shown only when there is one - on a
	// filesystem without compression the two numbers are nearly equal and the pair
	// would be noise.
	shape := ""
	if u.ApparentBytes > 0 && u.Bytes > 0 {
		ratio := float64(u.ApparentBytes) / float64(u.Bytes)
		if ratio < 0.97 || ratio > 1.03 {
			shape = fmt.Sprintf(` Files are %s long but occupy %s on disk (%.0f%%), the difference `+
				`being filesystem compression and block rounding.`,
				fmtBytes(u.ApparentBytes), fmtBytes(u.Bytes), 100/ratio)
		}
	}
	fmt.Fprintf(b, `<p class="sub">Measured every <code>CACHE_STATS_INTERVAL</code> by walking `+
		`<code>CACHE_PATH</code>; each point is the last measurement of that day. Size is the space `+
		`actually allocated (<code>st_blocks</code>), not the sum of file lengths.%s Free space is `+
		`<code>statfs</code> <em>Bavail</em>, so it excludes the reserve only root can use.%s</p>`+
		`<p class="sub" style="margin-top:-14px">On btrfs, treat free space as an estimate: it moves `+
		`with the data profile, and a filesystem can hit <code>ENOSPC</code> through exhausted `+
		`metadata chunks while this number still looks comfortable. Falling free space here is a `+
		`reliable signal; a healthy-looking number is not a guarantee.</p>`, shape, budget)

	b.WriteString(`<div class="row3">`)

	budgetMiB := float64(s.CacheMaxBytes) / mib
	svg1, _ := linesPanel([][]float64{s.CacheBytes}, []string{"c-ip"}, "Cache size on disk", budgetMiB, s.Smooth)
	card(b, "cache size, MiB", fmt.Sprintf("now <b>%s</b>", fmtBytes(u.Bytes)),
		svg1, seriesJSON(s.Days, s.CacheBytes), jsonStrings("MiB"), jsonStrings("--served"), "")

	svg2, _ := linesPanel([][]float64{s.FsFree}, []string{"c-bid"}, "Free space on the partition", 0, s.Smooth)
	free := "no statfs"
	if u.FsTotal > 0 {
		free = fmt.Sprintf("now <b>%s</b> free", fmtBytes(u.FsFree))
	}
	card(b, "partition free, MiB", free,
		svg2, seriesJSON(s.Days, s.FsFree), jsonStrings("MiB free"), jsonStrings("--ident"), "")

	svg3, _ := linesPanel([][]float64{s.CacheTmp}, []string{"c-ip"}, "Abandoned temp files", 0, s.Smooth)
	card(b, "abandoned .tmp, MiB", fmt.Sprintf("now <b>%s</b>", fmtBytes(u.TmpBytes)),
		svg3, seriesJSON(s.Days, s.CacheTmp), jsonStrings("MiB"), jsonStrings("--served"), "")

	b.WriteString(`</div>`)
}

// --- small numeric helpers ---

func toFloats(v []uint64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	return out
}

func colOK(p []probeDay) []uint64 {
	out := make([]uint64, len(p))
	for i, x := range p {
		out[i] = x.OK
	}
	return out
}

func colFail(p []probeDay) []uint64 {
	out := make([]uint64, len(p))
	for i, x := range p {
		out[i] = x.Fail
	}
	return out
}

func pairCol(p [][2]float64, k int) []float64 {
	out := make([]float64, len(p))
	for i, x := range p {
		out[i] = x[k]
	}
	return out
}

func maxPairHi(p [][2]float64) float64 {
	var m float64
	for _, v := range p {
		if v[1] > m {
			m = v[1]
		}
	}
	if m == 0 {
		return 1
	}
	return m
}

func ratio(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		if b[i] > 0 {
			out[i] = a[i] / b[i]
		}
	}
	return out
}

// median skips zeros: a day with no traffic is an absent measurement, not a
// measurement equal to zero, and counting it would drag the result down the further
// the range extends.
func median(v []float64) float64 {
	nonZero := make([]float64, 0, len(v))
	for _, x := range v {
		if x > 0 {
			nonZero = append(nonZero, x)
		}
	}
	if len(nonZero) == 0 {
		return 0
	}
	for i := 1; i < len(nonZero); i++ {
		for j := i; j > 0 && nonZero[j] < nonZero[j-1]; j-- {
			nonZero[j], nonZero[j-1] = nonZero[j-1], nonZero[j]
		}
	}
	return nonZero[len(nonZero)/2]
}

func maxOf(v []float64) float64 {
	var m float64
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}
