package engine

// SetDefaultDownloadDir changes the folder new downloads use when they don't
// name one. Downloads already added keep their folder. It is safe to call
// while other goroutines add downloads.
func (e *Engine) SetDefaultDownloadDir(dir string) {
	e.mu.Lock()
	e.defaultDownloadDir = dir
	e.mu.Unlock()
}

// DefaultDownloadDir is the folder new downloads use when they don't name one.
func (e *Engine) DefaultDownloadDir() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.defaultDownloadDir
}
