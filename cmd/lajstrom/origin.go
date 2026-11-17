package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/lajstrom/lajstrom/internal/api"
	"github.com/lajstrom/lajstrom/internal/auth"
)

// Who ran a command that writes audit rows against the database (docs/spec/rbac.md §6): the
// variables of --actor and --reason, and the bounds of their values in bytes.
const (
	envActor  = "LAJSTROM_ACTOR"
	envReason = "LAJSTROM_REASON"
	maxActor  = 128
	maxReason = auth.MaxChangeReason
)

// auditedCommands are the commands that write audit rows against the database, by their path
// below lajstrom, each with whether --reason is the reason of the change (docs/spec/rbac.md §6):
// not on admin policy rule create, whose --reason is the rule's, which clients see; the reason of
// the change comes from LAJSTROM_REASON there. A command that goes over HTTP (admin bootstrap,
// admin retention, admin prefetch) is the principal of its credential, and is not here.
var auditedCommands = map[string]bool{
	"reindex":                  true,
	"admin repo create":        true,
	"admin repo delete":        true,
	"admin key rotate":         true,
	"admin key drop":           true,
	"admin user create":        true,
	"admin user set-owner":     true,
	"admin user disable":       true,
	"admin user enable":        true,
	"admin user rename":        true,
	"admin user delete":        true,
	"admin user set-password":  true,
	"admin token create":       true,
	"admin token revoke":       true,
	"admin binding create":     true,
	"admin binding delete":     true,
	"admin webhook create":     true,
	"admin webhook update":     true,
	"admin webhook delete":     true,
	"admin webhook rotate":     true,
	"admin snapshot create":    true,
	"admin snapshot restore":   true,
	"admin policy rule create": false,
	"admin policy rule delete": true,
	"admin ci-policy create":   true,
	"admin ci-policy delete":   true,
}

// withOrigins applies withOrigin to the commands of auditedCommands below root, and panics on one
// that root does not have, which a test catches.
func (a *app) withOrigins(root *cobra.Command) {
	for path, reason := range auditedCommands {
		cmd, rest, err := root.Find(strings.Fields(path))
		if err != nil || len(rest) > 0 || cmd.CommandPath() != root.Name()+" "+path {
			panic("lajstrom: auditedCommands: no command " + path)
		}
		a.withOrigin(cmd, reason)
	}
}

// principalKinds are the kinds of principal of docs/spec/rbac.md §1: an --actor of the form
// <kind>:… is refused, so that no reader takes it for the row's principal.
var principalKinds = []auth.Kind{auth.KindUser, auth.KindToken, auth.KindGroup, auth.KindCert, auth.KindKubernetes, auth.KindCI}

// withOrigin gives cmd, a command that writes audit rows against the database, the flags --actor
// and, unless the command has a --reason of its own (admin policy rule create, whose reason is
// the rule's), --reason. Its PreRunE reads them, or their variables, checks them before anything
// is written, and stores the command line's origin in a and in the command's context
// (auth.WithOrigin), where the writers that take no Origin find it.
func (a *app) withOrigin(cmd *cobra.Command, reason bool) *cobra.Command {
	cmd.Flags().StringVar(&a.actor, "actor", "", "who runs this, as your organisation names people (an e-mail address, a login); "+
		"a label in the audit rows, never a principal (env "+envActor+")")
	if reason {
		cmd.Flags().StringVar(&a.reason, "reason", "", "why: a ticket or a sentence, in the audit rows as changeReason (env "+envReason+")")
	}
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		o, err := a.loadOrigin()
		if err != nil {
			return err
		}
		cmd.SetContext(auth.WithOrigin(cmd.Context(), o))
		return nil
	}
	return cmd
}

// loadOrigin sets a.origin: the source cli, the host, the operating-system user, and the actor and
// the reason of the flags, else of their variables (an empty one is unset). A value out of its
// bounds is a usage error, exit status 2.
func (a *app) loadOrigin() (api.Origin, error) {
	actor, reason := a.actor, a.reason
	if actor == "" {
		actor, _ = a.lookupEnv(envActor)
	}
	if reason == "" {
		reason, _ = a.lookupEnv(envReason)
	}
	if err := checkLabel(actor, maxActor); err != nil {
		return api.Origin{}, usageError("--actor (%s): %v", envActor, err)
	}
	if kind, _, ok := strings.Cut(actor, ":"); actor == string(auth.KindAnonymous) || ok && isPrincipalKind(kind) {
		return api.Origin{}, usageError("--actor (%s): %q has the form of a principal; name the person as your organisation does, "+
			"an e-mail address or a login", envActor, actor)
	}
	if err := checkLabel(reason, maxReason); err != nil {
		return api.Origin{}, usageError("--reason (%s): %v", envReason, err)
	}
	o := baseOrigin()
	o.Actor, o.ChangeReason = actor, reason
	a.origin = &o
	return o, nil
}

// cliOrigin is the origin of a change that lajstrom admin makes against the database: the
// source cli, the host name of the machine it runs on and the operating-system user it runs as,
// which its audit rows carry, so that a recovery of a lost admin names where and as whom it ran
// (#440, #1284); with the actor and the reason of withOrigin's flags once its PreRunE ran.
func (a *app) cliOrigin() api.Origin {
	if a.origin != nil {
		return *a.origin
	}
	return baseOrigin()
}

// baseOrigin is the origin of the command line without an actor or a reason.
func baseOrigin() api.Origin {
	return api.Origin{Source: api.SourceCLI, Host: hostName(), OSUser: osUser()}
}

// hostName is the host name of the machine, "" when the system does not tell it.
func hostName() string {
	host, _ := os.Hostname()
	return host
}

// osUser is the name of the operating-system user the process runs as, by its uid in the
// system's user database, or uid:<n> where the database has no name for it (a container's
// arbitrary uid). Not $USER, which the caller sets.
func osUser() string {
	uid := strconv.Itoa(os.Getuid())
	if u, err := user.LookupId(uid); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid:" + uid
}

// checkLabel returns why s is not a value of at most limit bytes of UTF-8 without control
// characters; nil for an empty s, which is no value.
func checkLabel(s string, limit int) error {
	switch {
	case len(s) > limit:
		return fmt.Errorf("%d bytes, at most %d", len(s), limit)
	case !utf8.ValidString(s):
		return errors.New("not UTF-8")
	case strings.ContainsFunc(s, unicode.IsControl):
		return errors.New("a control character (a line break, a tab)")
	}
	return nil
}

// isPrincipalKind reports whether kind is the kind of a principal of docs/spec/rbac.md §1.
func isPrincipalKind(kind string) bool { return slices.Contains(principalKinds, auth.Kind(kind)) }
