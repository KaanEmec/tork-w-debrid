package health

// FreeBytes reports the space available to this user on the filesystem
// holding dir. It is the same check doctor's disk line uses; the settings
// screen shows it next to the download folder.
func FreeBytes(dir string) (int64, error) { return freeBytes(dir) }
