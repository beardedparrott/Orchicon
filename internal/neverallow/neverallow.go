// Package neverallow declares the binary and command class that is NEVER allowed to run — not
// inside a worker execution, and not inside an interactive Ask Orchicon session either.
package neverallow

import "strings"

// Binary is one member of the never-allow binary class.
type Binary struct {
	// Name is the binary's name as it appears on PATH, or a shell case pattern
	// (`mkfs.*`) for a family member that has no single canonical basename.
	Name string
	// Shim reports whether the execution guard should install a PATH symlink named after this member.
	Shim bool
}

// Binaries is the class's binary membership, in declaration order.
var Binaries = []Binary{
	{Name: "sudo", Shim: true},
	{Name: "dd", Shim: true},
	{Name: "mkfs", Shim: true},
	{Name: "mkfs.ext2", Shim: true},
	{Name: "mkfs.ext3", Shim: true},
	{Name: "mkfs.ext4", Shim: true},
	{Name: "mkfs.xfs", Shim: true},
	{Name: "mkfs.btrfs", Shim: true},
	{Name: "mkfs.fat", Shim: true},
	{Name: "mkfs.vfat", Shim: true},
	{Name: "mkswap", Shim: true},
	{Name: "fdisk", Shim: true},
	{Name: "parted", Shim: true},
	{Name: "shred", Shim: true},
	{Name: "wipefs", Shim: true},
	{Name: "pvcreate", Shim: true},
	{Name: "pvremove", Shim: true},
	{Name: "vgcreate", Shim: true},
	{Name: "vgremove", Shim: true},
	{Name: "lvcreate", Shim: true},
	{Name: "lvremove", Shim: true},
	// Pattern-only family entries: no symlink is created for these (no such binary exists on PATH), but
	// the guard's `case` arm needs them so a member this list did not name exactly is still refused.
	{Name: "mkfs.*"},
	{Name: "fdisk*"},
	{Name: "wipefs*"},
}

// CommandPatterns is the class's bash membership: the patterns opencode matches against the exact
// command string the Bash tool runs.
var CommandPatterns = []string{
	// sudo — escalate to a root shell is never needed in-project.
	"sudo", "sudo *", "sudo su *", "sudo -i *", "sudo -s *", "sudo bash *",
	"sudo sh *", "sudo rm *", "sudo * rm *", "sudo -u * rm *",
	// shell-construct smuggling variants.
	"(* sudo *", "{* sudo *",
	"* & sudo *", "* && sudo *", "* ; sudo *", "* | sudo *",
	// disk / partition / LVM / wipe tools.
	"mkfs*", "mkfs.*", "fdisk*", "parted *", "shred *", "wipefs*",
	"mkswap *", "swapoff *", "swapon *",
	"pvcreate *", "pvremove *", "vgcreate *", "vgremove *", "vgextend *",
	"lvcreate *", "lvremove *", "lvreduce *", "lvextend *",
	"dd if=* of=/dev/*", "dd * of=/dev/*", "dd of=/dev/*",
	"* & dd *", "* && dd *", "* ; dd *",
}

// Shimmed returns the binary names the OS-level execution guard installs a PATH shim for — the members backed by
// a real binary — in declaration order.
func Shimmed() []string {
	out := make([]string, 0, len(Binaries))
	for _, b := range Binaries {
		if b.Shim {
			out = append(out, b.Name)
		}
	}
	return out
}

// CasePattern returns the `case` alternative list the execution guard's shim renders: every
// declared name joined with "|".
func CasePattern() string {
	names := make([]string, 0, len(Binaries))
	for _, b := range Binaries {
		names = append(names, b.Name)
	}
	return strings.Join(names, "|")
}

// DenyRules returns the class's bash patterns as a COPY, so a consumer that appends its own rules (the worker
// profile appends its project-boundary denies) cannot mutate the shared declaration.
func DenyRules() []string {
	out := make([]string, len(CommandPatterns))
	copy(out, CommandPatterns)
	return out
}
