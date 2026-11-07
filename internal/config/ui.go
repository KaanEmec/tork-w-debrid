package config

import "strconv"

// UIConfig holds Settings › Look (J8.7).
type UIConfig struct {
	// Motion is the "animations" switch. Off stops every animation at once;
	// --no-motion, NO_COLOR and TERM=dumb turn motion off for a run anyway.
	Motion bool `yaml:"motion"`
	// Intro plays the intro once after each update (J8.8). "play the intro"
	// in Settings plays it either way.
	Intro bool `yaml:"intro"`
}

// DefaultUIConfig is the ui block a fresh config gets: both switches on.
func DefaultUIConfig() UIConfig { return UIConfig{Motion: true, Intro: true} }

// WriteUIMotion saves ui.motion, keeping comments, unrelated keys and the
// file mode. The caller updates its in-memory config after success.
func (c *Config) WriteUIMotion(on bool) error {
	return c.writeScalar([]string{"ui", "motion"}, "!!bool", strconv.FormatBool(on))
}

// WriteUIIntro saves ui.intro the same way as WriteUIMotion.
func (c *Config) WriteUIIntro(on bool) error {
	return c.writeScalar([]string{"ui", "intro"}, "!!bool", strconv.FormatBool(on))
}
