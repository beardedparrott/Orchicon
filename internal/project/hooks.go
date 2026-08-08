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

// NotifyProjectChanged fires the registered callback, if any. Exported so
// other services whose changes feed the same mount manifest (work item
// context_files, which the single-container mount manifest also unions)
// can trigger an immediate refresh without waiting for the periodic writer.
func NotifyProjectChanged() { notifyProjectChanged() }

// notifyProjectChanged fires the registered callback, if any.
func notifyProjectChanged() {
	if onProjectChanged != nil {
		onProjectChanged()
	}
}
