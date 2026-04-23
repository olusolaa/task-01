package money

import (
	"errors"
	"testing"
)

func TestParseKobo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    Kobo
		wantErr error
	}{
		{in: "1", want: 1},
		{in: "10", want: 10},
		{in: "10000", want: 10000},
		{in: "100000000", want: 100000000},
		{in: "9223372036854775807", want: 9223372036854775807},

		{in: "", wantErr: ErrEmpty},

		{in: "0", wantErr: ErrNonPositive},
		{in: "-1", wantErr: ErrNotInteger},
		{in: "+1", wantErr: ErrNotInteger},

		{in: "01", wantErr: ErrNotInteger},
		{in: "007", wantErr: ErrNotInteger},

		{in: "10.00", wantErr: ErrNotInteger},
		{in: "10.", wantErr: ErrNotInteger},
		{in: ".10", wantErr: ErrNotInteger},
		{in: "1e4", wantErr: ErrNotInteger},
		{in: "1E4", wantErr: ErrNotInteger},
		{in: "1_000", wantErr: ErrNotInteger},
		{in: "1,000", wantErr: ErrNotInteger},
		{in: " 10", wantErr: ErrNotInteger},
		{in: "10 ", wantErr: ErrNotInteger},
		{in: "\t10", wantErr: ErrNotInteger},
		{in: "10\n", wantErr: ErrNotInteger},
		{in: "0x10", wantErr: ErrNotInteger},
		{in: "abc", wantErr: ErrNotInteger},
		{in: "10abc", wantErr: ErrNotInteger},

		{in: "9223372036854775808", wantErr: ErrOutOfRange},
		{in: "99999999999999999999", wantErr: ErrOutOfRange},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseKobo(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseKobo(%q): err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKobo(%q): unexpected err %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseKobo(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestKoboFormatting(t *testing.T) {
	t.Parallel()
	if got := Kobo(10000).String(); got != "10000" {
		t.Fatalf("String() = %q, want %q", got, "10000")
	}
	if got := Kobo(0).String(); got != "0" {
		t.Fatalf("String() = %q, want %q", got, "0")
	}
	if got := Kobo(10000).Int64(); got != 10000 {
		t.Fatalf("Int64() = %d, want 10000", got)
	}
}
