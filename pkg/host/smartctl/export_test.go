package smartctl

// WithEvalSymlinks replaces the symlink resolver, to reproduce a drive that
// vanishes between the listing and its resolution.
func (r *Runner) WithEvalSymlinks(f func(string) (string, error)) *Runner {
	r.evalSymlinks = f

	return r
}
