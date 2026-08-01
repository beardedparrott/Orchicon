package project

// onProjectChanged is invoked after a project's directory-bearing fields
// (project_dir, context_files) change. The server registers it to refresh
// the single-container mount manifest immediately (see
// internal/server/project_mounts.go) so a saved project dir is picked up
// without waiting for the periodic writer.
var onProjectChanged func()

// SetOnProjectChanged registers the callback fired after a project's
// directory-bearing fields change.
func SetOnProjectChanged(fn func()) { onProjectChanged = fn }

// notifyProjectChanged fires the registered callback, if any.
func notifyProjectChanged() {
	if onProjectChanged != nil {
		onProjectChanged()
	}
}
