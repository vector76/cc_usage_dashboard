package folder

import "testing"

func TestKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{``, ``},
		{`  `, ``},
		// Windows paths compare case-insensitively, with either separator,
		// and without a trailing separator.
		{`C:\WebApps\Foo`, `c:\webapps\foo`},
		{`c:\webapps\foo\`, `c:\webapps\foo`},
		{`C:/WebApps/Foo`, `c:\webapps\foo`},
		{`C:\`, `c:`},
		{`\\server\share\Dir`, `\\server\share\dir`},
		// POSIX paths are case-sensitive; only the trailing slash goes.
		{`/home/User/Proj/`, `/home/User/Proj`},
		{`/`, `/`},
		// An encoded directory name is kept verbatim.
		{`C--WebApps-cc-usage-dashboard`, `C--WebApps-cc-usage-dashboard`},
	}
	for _, c := range cases {
		if got := Key(c.in); got != c.want {
			t.Errorf("Key(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEncode(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\WebApps\cc_usage_dashboard`, `C--WebApps-cc-usage-dashboard`},
		{`/home/user/my.proj`, `-home-user-my-proj`},
		{`R:\GVCS Independent Model`, `R--GVCS-Independent-Model`},
	}
	for _, c := range cases {
		if got := Encode(c.in); got != c.want {
			t.Errorf("Encode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsEncoded(t *testing.T) {
	for in, want := range map[string]bool{
		``:               false,
		`C--Jewel-agent`: true,
		`-home-user-x`:   true,
		`C:\Jewel\agent`: false,
		`/home/user/x`:   false,
	} {
		if got := IsEncoded(in); got != want {
			t.Errorf("IsEncoded(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolverLongestPrefixWins(t *testing.T) {
	r := NewResolver([]Assignment{
		{Folder: `C:\Jewel\agent`, ProjectID: 1},
		{Folder: `C:\Jewel\agent\docs\dsp-review`, ProjectID: 2},
		{Folder: `C:\OCIS2\BNA`, ProjectID: 3},
		{Folder: `/home/u/proj`, ProjectID: 4},
		{Folder: `C--Legacy-encoded`, ProjectID: 5},
	})
	cases := []struct {
		in      string
		want    int64 // 0 = unassigned
		wantVia string
	}{
		{`C:\Jewel\agent`, 1, `C:\Jewel\agent`},
		{`C:\Jewel\agent\src`, 1, `C:\Jewel\agent`},
		{`C:\Jewel\agent\docs\dsp-review`, 2, `C:\Jewel\agent\docs\dsp-review`},
		{`C:\Jewel\agent\docs\dsp-review\fix-campaign`, 2, `C:\Jewel\agent\docs\dsp-review`},
		{`c:\ocis2\bna\.claude\worktrees\Alice\app`, 3, `C:\OCIS2\BNA`},
		{`C:\OCIS2\BNA-other`, 0, ``}, // segment boundary
		{`C:\Jewel`, 0, ``},           // ancestors are not covered
		{`/home/u/proj/sub`, 4, `/home/u/proj`},
		{`/home/U/proj/sub`, 0, ``}, // POSIX is case-sensitive
		{`C--Legacy-encoded`, 5, `C--Legacy-encoded`},
		{`C--Legacy-encoded-more`, 0, ``}, // encoded names match exactly
		{``, 0, ``},
	}
	for _, c := range cases {
		a, ok := r.Resolve(c.in)
		if c.want == 0 {
			if ok {
				t.Errorf("Resolve(%q) = %+v, want unassigned", c.in, a)
			}
			continue
		}
		if !ok || a.ProjectID != c.want || a.Folder != c.wantVia {
			t.Errorf("Resolve(%q) = %+v, %v; want project %d via %q", c.in, a, ok, c.want, c.wantVia)
		}
	}
}

func TestResolverRootAssignment(t *testing.T) {
	r := NewResolver([]Assignment{{Folder: `R:\`, ProjectID: 7}, {Folder: `/`, ProjectID: 8}})
	if a, ok := r.Resolve(`R:\UCAS3\x`); !ok || a.ProjectID != 7 {
		t.Errorf("drive root: got %+v, %v", a, ok)
	}
	if a, ok := r.Resolve(`/srv/x`); !ok || a.ProjectID != 8 {
		t.Errorf("posix root: got %+v, %v", a, ok)
	}
}
