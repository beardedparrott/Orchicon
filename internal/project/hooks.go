package project

// onProjectChanged is invoked after a project's directory-bearing fields (project_dir,
// context_files) change.
var onProjectChanged func()

// SetOnProjectChanged registers the callback fired after a project's
// directory-bearing fields change.
func SetOnProjectChanged(fn func()) { onProjectChanged = fn }

// NotifyProjectChanged fires the registered callback, if any.
func NotifyProjectChanged() { notifyProjectChanged() }

// notifyProjectChanged fires the registered callback, if any.
func notifyProjectChanged() {
	if onProjectChanged != nil {
		onProjectChanged()
	}
}
