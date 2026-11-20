package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var pkgGlibc = ubuntuPackage{Component: "main", Source: "glibc", Binary: "libc6-dbg"}

func TestSplitDebName(t *testing.T) {
	for name, want := range map[string][2]string{
		"libc6-dbg_2.43-2ubuntu2.7_amd64.deb":   {"2.43-2ubuntu2.7", "amd64"},
		"libc6-dbg_2.43-2ubuntu2_amd64v3.deb":   {"2.43-2ubuntu2", "amd64v3"},
		"libc6-dbg_1:2.3+really2.2-1_arm64.deb": {"1:2.3+really2.2-1", "arm64"},
	} {
		v, a, ok := splitDebName(name, "libc6-dbg")
		if !ok || v != want[0] || a != want[1] {
			t.Errorf("%s: got %q %q %v", name, v, a, ok)
		}
	}
	// The glibc pool directory holds twenty other binaries; libc6 must not
	// match libc6-dbg, nor libc6-dbg match libc6-dbgsym.
	for _, name := range []string{"libc6_2.43-2ubuntu2_amd64.deb", "libc6-dbgsym_2.23-0ubuntu3_amd64.ddeb", "libc6-dbg_2.43.dsc"} {
		if _, _, ok := splitDebName(name, "libc6-dbg"); ok {
			t.Errorf("%s matched libc6-dbg", name)
		}
	}
}

func TestPoolShard(t *testing.T) {
	for src, want := range map[string]string{"glibc": "g", "libxml2": "libx", "lib": "l", "ncurses": "n"} {
		if got := poolShard(src); got != want {
			t.Errorf("%s: got %q, want %q", src, got, want)
		}
	}
}

func TestListPoolKeepsOnlyTheBinaryAndArches(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ubuntu/pool/main/g/glibc/" {
			http.NotFound(w, r)
			return
		}
		for _, f := range []string{
			"libc6-dbg_2.43-2ubuntu2.7_amd64.deb", "libc6-dbg_2.43-2ubuntu2.7_s390x.deb",
			"libc6_2.43-2ubuntu2.7_amd64.deb", "glibc_2.43-2ubuntu2.7.dsc",
			"libc6-dbg_2.39-0ubuntu8%2B1_i386.deb",
		} {
			fmt.Fprintf(w, "<a href=\"%s\">%s</a>\n", f, f)
		}
	}))
	defer ts.Close()

	got, err := listPool(context.Background(), ts.Client(), ts.URL+"/ubuntu/", pkgGlibc, map[string]bool{"amd64": true, "i386": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d archives: %+v", len(got), got)
	}
	a := got[0]
	if a.URL != ts.URL+"/ubuntu/pool/main/g/glibc/libc6-dbg_2.43-2ubuntu2.7_amd64.deb" || a.Kind != kindDebuginfo || a.Format != "deb" {
		t.Errorf("unexpected archive %+v", a)
	}
	if a.Fallback != launchpadFiles+"libc6-dbg_2.43-2ubuntu2.7_amd64.deb" {
		t.Errorf("a pool file must carry its Launchpad url, or it 404s after the next SRU: %q", a.Fallback)
	}
	if got[1].Version != "2.39-0ubuntu8+1" {
		t.Errorf("escaped version not decoded: %q", got[1].Version)
	}
}

// TestHeaderIsWhatDebDebuginfodParses pins the contract with
// cmd/deb-debuginfod: the header must end in component/shard/source/file, or
// sources for these build ids silently stop working.
func TestHeaderIsWhatDebDebuginfodParses(t *testing.T) {
	a := ubuntuArchive(pkgGlibc, "main", "2.43-2ubuntu2.7", "amd64", "https://example/x.deb")
	want := "/ubuntu/pool/main/g/glibc/libc6-dbg_2.43-2ubuntu2.7_amd64.deb"
	if a.Header != want {
		t.Fatalf("header %q, want %q", a.Header, want)
	}
}

func TestListLaunchpadPagesDedupesAndSkipsThePool(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := func(ver, series, arch string) string {
			return fmt.Sprintf(`{"binary_package_version":%q,"distro_arch_series_link":"https://api.launchpad.net/1.0/ubuntu/%s/%s","component_name":"main"}`, ver, series, arch)
		}
		if r.URL.Query().Get("page") == "" {
			// The same version is published in several pockets and series.
			fmt.Fprintf(w, `{"entries":[%s,%s,%s],"next_collection_link":%q}`,
				entry("2.35-0ubuntu3.1", "jammy", "amd64"),
				entry("2.35-0ubuntu3.1", "jammy", "amd64"),
				entry("2.35-0ubuntu3.1", "jammy", "s390x"),
				ts.URL+"/?page=2")
			return
		}
		fmt.Fprintf(w, `{"entries":[%s,%s]}`,
			entry("2.35-0ubuntu3.2", "jammy", "amd64"),
			entry("2.43-2ubuntu2.7", "resolute", "amd64"))
	}))
	defer ts.Close()

	// Point the listing at the fake API for the duration of the test.
	old := launchpadAPIURL
	launchpadAPIURL = ts.URL + "/"
	defer func() { launchpadAPIURL = old }()

	skip := map[string]bool{"libc6-dbg_2.43-2ubuntu2.7_amd64.deb": true}
	got, err := listLaunchpad(context.Background(), ts.Client(), pkgGlibc, map[string]bool{"amd64": true}, skip, seriesWindow{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Name)
		if !strings.HasPrefix(a.URL, launchpadFiles) {
			t.Errorf("%s: fetched from %s, want Launchpad", a.Name, a.URL)
		}
	}
	want := "libc6-dbg_2.35-0ubuntu3.1_amd64.deb,libc6-dbg_2.35-0ubuntu3.2_amd64.deb"
	if strings.Join(names, ",") != want {
		t.Fatalf("got %v, want %s", names, want)
	}
}

func TestParseUbuntuPackages(t *testing.T) {
	got, err := parseUbuntuPackages("main/glibc/libc6-dbg, universe/foo/foo-dbg")
	if err != nil || len(got) != 2 || got[1] != (ubuntuPackage{"universe", "foo", "foo-dbg"}) {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := parseUbuntuPackages("libc6-dbg"); err == nil {
		t.Fatal("a bare binary name must be rejected: the pool directory and the component are unknown")
	}
}

// TestPoolFileIsRecordedUnderBothURLs: the second row is what keeps a build id
// servable after the pool drops its version, without waiting for a re-index.
func TestPoolFileIsRecordedUnderBothURLs(t *testing.T) {
	const id = "9851d79b1f32e900a27f8d0a68e29d750303f0c6"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(debWithBuildID(t, id))
	}))
	defer ts.Close()

	a := ubuntuArchive(pkgGlibc, "main", "2.43-2ubuntu2.7", "amd64", ts.URL+"/pool/x.deb")
	a.Fallback = launchpadFiles + a.Name
	r := scanOne(context.Background(), ts.Client(), a, &counters{})
	if r.archive.Err != "" {
		t.Fatal(r.archive.Err)
	}
	var urls []string
	for _, row := range r.arts {
		if row.BuildID != id {
			t.Fatalf("unexpected build id %s", row.BuildID)
		}
		if row.Archive.Header != a.Header {
			t.Errorf("fallback row has header %q", row.Archive.Header)
		}
		urls = append(urls, row.Archive.URL)
	}
	if strings.Join(urls, " ") != a.URL+" "+a.Fallback {
		t.Fatalf("rows for %v, want pool then Launchpad", urls)
	}
	if r.archive.Archive.URL != a.URL {
		t.Fatalf("resume log keyed on %s, want the url actually downloaded", r.archive.Archive.URL)
	}
}

// debWithBuildID builds a minimal .deb holding one separate debug file.
func debWithBuildID(t *testing.T, id string) []byte {
	t.Helper()
	data := tarGz(t, map[string][]byte{
		"./usr/lib/debug/.build-id/" + id[:2] + "/" + id[2:] + ".debug": elfWithID(t, id),
	})
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	member := func(name string, body []byte) {
		fmt.Fprintf(&buf, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(body))
		buf.Write(body)
		if len(body)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("data.tar.gz", data)
	return buf.Bytes()
}

// TestHistoryStartsAtTheFloorSeries: -ubuntu-since 20.04 must keep focal and
// everything after it - including a series whose version sorts lower as a
// string - and cut the rest both on the server and here.
func TestHistoryStartsAtTheFloorSeries(t *testing.T) {
	var sinceParam string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/series" {
			fmt.Fprint(w, `{"entries":[
				{"name":"resolute","version":"26.04","date_created":"2025-10-09T10:46:47Z"},
				{"name":"jammy","version":"22.04","date_created":"2021-10-14T00:00:00Z"},
				{"name":"focal","version":"20.04","date_created":"2019-02-18T08:38:45Z"},
				{"name":"bionic","version":"18.04","date_created":"2017-10-19T00:00:00Z"},
				{"name":"warty","version":"4.10","date_created":"2004-08-20T00:00:00Z"}]}`)
			return
		}
		sinceParam = r.URL.Query().Get("created_since_date")
		entry := func(ver, series string) string {
			return fmt.Sprintf(`{"binary_package_version":%q,"distro_arch_series_link":"https://api.launchpad.net/1.0/ubuntu/%s/amd64","component_name":"main"}`, ver, series)
		}
		// A bionic SRU published after focal's window opened: the date cut
		// lets it through, the series filter must not.
		fmt.Fprintf(w, `{"entries":[%s,%s,%s,%s]}`,
			entry("2.31-0ubuntu9.2", "focal"), entry("2.35-0ubuntu3.1", "jammy"),
			entry("2.27-3ubuntu1.6", "bionic"), entry("2.43-2ubuntu2", "resolute"))
	}))
	defer ts.Close()
	oldSeries, oldAPI := launchpadSeriesURL, launchpadAPIURL
	launchpadSeriesURL, launchpadAPIURL = ts.URL+"/series", ts.URL+"/api"
	defer func() { launchpadSeriesURL, launchpadAPIURL = oldSeries, oldAPI }()

	w, err := ubuntuSeriesSince(context.Background(), ts.Client(), "20.04")
	if err != nil {
		t.Fatal(err)
	}
	got, err := listLaunchpad(context.Background(), ts.Client(), pkgGlibc, map[string]bool{"amd64": true}, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	if sinceParam != "2019-02-18" {
		t.Errorf("created_since_date = %q, want focal's creation date", sinceParam)
	}
	var vers []string
	for _, a := range got {
		vers = append(vers, a.Version)
	}
	if strings.Join(vers, ",") != "2.31-0ubuntu9.2,2.35-0ubuntu3.1,2.43-2ubuntu2" {
		t.Fatalf("got %v; bionic must be cut, focal and newer kept", vers)
	}

	if _, err := ubuntuSeriesSince(context.Background(), ts.Client(), "focal"); err == nil {
		t.Error("a series name instead of a version must be rejected")
	}
}

func TestParseSeriesVersion(t *testing.T) {
	a, _ := parseSeriesVersion("4.10")
	b, _ := parseSeriesVersion("20.04")
	if a >= b {
		t.Fatal("4.10 must sort before 20.04")
	}
}
