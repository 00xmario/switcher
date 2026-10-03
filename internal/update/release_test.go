package update

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func releaseWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDMGRetriesFailAfterExhaustion(t *testing.T) {
	workflow := releaseWorkflow(t)
	_, step, ok := strings.Cut(workflow, "      - name: Package drag-to-Applications DMG\n")
	if !ok {
		t.Fatal("DMG packaging step not found")
	}
	step, _, _ = strings.Cut(step, "\n      - name:")
	var shell strings.Builder
	for _, line := range strings.Split(step, "\n") {
		if body, ok := strings.CutPrefix(line, "          "); ok {
			shell.WriteString(body + "\n")
		}
	}
	start := strings.Index(shell.String(), "for attempt in 1 2 3; do\n")
	if start < 0 {
		t.Fatal("DMG retry loop not found")
	}
	loop := shell.String()[start:]
	for _, tc := range []struct {
		name      string
		successAt int
		attempts  int
		wantError bool
	}{
		{"first attempt", 1, 1, false},
		{"third attempt", 3, 3, false},
		{"exhausted", 4, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Only the actual workflow loop runs. All external commands in it
			// are shell functions, so no disk image or application is created.
			prefix := fmt.Sprintf(`attempts=0
VERSION=fixture
STAGE=fixture
hdiutil() { attempts=$((attempts+1)); printf 'attempt:%%s\n' "$attempts"; [ "$attempts" -ge %d ]; }
sleep() { printf 'sleep\n'; }
`, tc.successAt)
			dir := t.TempDir()
			cmd := exec.Command("/bin/bash", "-e", "-u", "-o", "pipefail", "-c", prefix+loop)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Errorf("error=%v, want failure=%v; output=%s", err, tc.wantError, out)
			}
			if got := strings.Count(string(out), "attempt:"); got != tc.attempts {
				t.Errorf("attempts=%d, want %d", got, tc.attempts)
			}
		})
	}
}

const goTOMLLicense = `The MIT License (MIT)

go-toml v2
Copyright (c) 2021 - 2023 Thomas Pelletier

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const goBSDLicense = `Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`

func TestLinkedDependencyNoticesContainCompleteLicenses(t *testing.T) {
	data, err := os.ReadFile("../../THIRD_PARTY_NOTICES.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		heading string
		license string
	}{
		{"go-toml v2", goTOMLLicense},
		{"golang.org/x/sys", goBSDLicense},
		{"golang.org/x/text", goBSDLicense},
	} {
		t.Run(tc.heading, func(t *testing.T) {
			_, section, ok := strings.Cut(string(data), "## "+tc.heading+"\n")
			if !ok {
				t.Fatalf("missing notice for %s", tc.heading)
			}
			section, _, _ = strings.Cut(section, "\n## ")
			if !strings.Contains(section, tc.license) {
				t.Errorf("%s lacks the complete upstream license", tc.heading)
			}
		})
	}
}

func TestNoticesRemainEmbeddedAndBundled(t *testing.T) {
	main, err := os.ReadFile("../../main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "//go:embed THIRD_PARTY_NOTICES.md") || !strings.Contains(string(main), "fmt.Print(thirdPartyNotices)") {
		t.Error("plain binary no longer exposes embedded notices")
	}
	if !strings.Contains(releaseWorkflow(t), `cp THIRD_PARTY_NOTICES.md "$APP/Contents/Resources/ThirdPartyNotices.md"`) {
		t.Error("macOS bundle omits notices")
	}
}
