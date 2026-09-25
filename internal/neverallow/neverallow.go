// Package neverallow declares the binary and command class that is NEVER allowed to run — not inside a worker
// execution, and not inside an interactive Ask Orchicon session either.
//
// WHY IT IS ITS OWN PACKAGE. The class is enforced at two layers that live in different packages and have
// nothing else in common: the OS-level execution guard (internal/guard) refuses the binaries unconditionally by
// shimming them on PATH, and the opencode config builder (internal/opencode) refuses the same commands at the
// permission layer. Both used to carry their OWN copy of the list, which is the shape "two lists that agree
// today" — they agree until someone edits one. The declaration lives here, imports nothing internal (the
// internal/askmode pattern), and every consumer reads it, so a change lands in all of them at once.
//
// WHAT IS HERE vs. WHAT IS NOT. This is the class that is never legitimate — sudo/dd escalation, disk, partition
// and LVM tooling, and the shell-construct variants that smuggle them past a prefix match. It is deliberately NOT
// the whole worker deny list: the project-boundary rules (destructive `rm` targets, root-wide chmod/chown,
// /dev/sd* redirection, download-and-execute) belong to the SANDBOX, not to the class, and stay with the worker
// profile that needs them. An interactive session keeps the class and drops the sandbox.
package neverallow

import "strings"

// Binary is one member of the never-allow binary class.
type Binary struct {
	// Name is the binary's name as it appears on PATH, or a shell case pattern
	// (`mkfs.*`) for a family member that has no single canonical basename.
	Name string
	// Shim reports whether the execution guard should install a PATH symlink
	// named after this member. Only real, resolvable basenames get a shim; a
	// pattern-only family entry exists so the guard's `case` arm still refuses
	// a differently-suffixed sibling of a shimmed name.
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
	// Pattern-only family entries: no symlink is created for these (no such
	// binary exists on PATH), but the guard's `case` arm needs them so a
	// member this list did not name exactly is still refused.
	{Name: "mkfs.*"},
	{Name: "fdisk*"},
	{Name: "wipefs*"},
}

// CommandPatterns is the class's bash membership: the patterns opencode matches against the exact command string
// the Bash tool runs. It covers the members above and the shell-construct variants that hide them behind a
// prefix (`* && sudo *`), so a smuggled invocation is denied by the same declaration that denies the bare one.
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

// CasePattern returns the `case` alternative list the execution guard's shim renders: every declared name joined
// with "|", so the family members are covered in the guard's own dispatch even though only the exact basenames
// get a symlink.
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
