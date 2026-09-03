package cmd

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/robertdewilde-dev/git-track/internal/hooks"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Probe whether the remote accepts the metadata ref namespace",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return err
		}
		remote := c.remote()
		ns := c.store.Namespace
		report := map[string]any{"remote": remote, "namespace": ns}

		url, err := c.git.Run("remote", "get-url", remote)
		if err != nil {
			return exitErr(ExitError, "remote %q is not configured", remote)
		}
		report["url"] = url
		info("remote:    %s (%s)", remote, url)
		info("namespace: %s", ns)

		// Probe: push a throwaway commit under the namespace, then delete it.
		treeSHA, err := c.git.RunStdin("", "mktree")
		if err != nil {
			return err
		}
		commitSHA, err := c.git.Run("commit-tree", treeSHA, "-m", "git-track doctor probe")
		if err != nil {
			return err
		}
		probeRef := fmt.Sprintf("%s/_git-track-probe-%08x", ns, rand.Uint32())
		_, pushErr := c.git.Run("push", "--quiet", "--no-verify", remote, commitSHA+":"+probeRef)
		usable := pushErr == nil
		if usable {
			if _, err := c.git.Run("push", "--quiet", "--no-verify", remote, ":"+probeRef); err != nil {
				info("warning: probe ref %s could not be deleted: %s", probeRef, err)
			}
		}
		report["namespaceUsable"] = usable

		fetchOK, pushOK := refspecsConfigured(c, remote, ns)
		report["fetchRefspec"] = fetchOK
		report["pushRefspec"] = pushOK

		hooksOK := false
		if dir, err := hooks.Dir(c.git); err == nil {
			hooksOK = hooks.IsOurs(dir + "/post-checkout")
		}
		report["hooks"] = hooksOK

		if flagJSON {
			return printJSON(report)
		}
		if usable {
			fmt.Printf("namespace %s: %s\n", ns, bold("usable"))
		} else {
			fmt.Printf("namespace %s: NOT accepted by the remote\n", ns)
			fmt.Println(strings.TrimSpace(`
The server rejected a push under this namespace (GitHub rejects most custom
ref namespaces; Gerrit reserves refs/meta/*). Try an alternative namespace:

    git config track.namespace refs/issue-meta/branches
    git track init
    git track doctor`))
		}
		status := func(ok bool) string {
			if ok {
				return "ok"
			}
			return "missing (run `git track init`)"
		}
		fmt.Printf("fetch refspec: %s\n", status(fetchOK))
		fmt.Printf("push refspec:  %s\n", status(pushOK))
		fmt.Printf("hooks:         %s\n", status(hooksOK))
		if !usable {
			return exitErr(ExitError, "")
		}
		return nil
	},
}

func refspecsConfigured(c *appCtx, remote, ns string) (fetchOK, pushOK bool) {
	fetchSpec := fmt.Sprintf("+%s/*:%s/*", ns, ns)
	for _, v := range c.git.ConfigAll(fmt.Sprintf("remote.%s.fetch", remote)) {
		if v == fetchSpec {
			fetchOK = true
		}
	}
	for _, v := range c.git.ConfigAll(fmt.Sprintf("remote.%s.push", remote)) {
		if v == ns+"/*" {
			pushOK = true
		}
	}
	return fetchOK, pushOK
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
