package anvil

import "testing"

func TestResolveStateSettings(t *testing.T) {
	no := false
	cases := []struct {
		name     string
		defaults *DefaultsConfig
		retain   int
		compress bool
		wantErr  bool
	}{
		{"omitted", nil, 3, true, false},
		{"empty state", &DefaultsConfig{State: &StateConfig{}}, 3, true, false},
		{"two", &DefaultsConfig{State: &StateConfig{RetainVersions: 2}}, 2, true, false},
		{"compress off", &DefaultsConfig{State: &StateConfig{CompressState: &no}}, 3, false, false},
		{"one", &DefaultsConfig{State: &StateConfig{RetainVersions: 1}}, 0, false, true},
		{"too many", &DefaultsConfig{State: &StateConfig{RetainVersions: 102}}, 0, false, true},
	}
	for _, c := range cases {
		r, comp, err := resolveStateSettings(c.defaults)
		if (err != nil) != c.wantErr || r != c.retain || comp != c.compress {
			t.Errorf("%s: got (%d, %v, %v), want (%d, %v, err=%v)", c.name, r, comp, err, c.retain, c.compress, c.wantErr)
		}
	}
}
