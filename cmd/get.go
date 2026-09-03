package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get [key]",
	Short: "Read one field or the whole document",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newCtx()
		if err != nil {
			return jsonError(err)
		}
		branch, err := c.branch()
		if err != nil {
			return jsonError(err)
		}
		doc, _, err := c.store.Read(branch)
		if err != nil {
			return jsonError(err)
		}
		if len(args) == 0 {
			if flagJSON {
				return printJSON(doc)
			}
			data, err := doc.Marshal()
			if err != nil {
				return err
			}
			fmt.Print(string(data))
			return nil
		}
		v, ok := doc.Get(args[0])
		if !ok {
			return jsonError(exitErr(ExitNoMetadata, "no such field: %s", args[0]))
		}
		if flagJSON {
			return printJSON(v)
		}
		switch val := v.(type) {
		case string:
			fmt.Println(val)
		case float64:
			if val == float64(int64(val)) {
				fmt.Println(int64(val))
			} else {
				fmt.Println(val)
			}
		default:
			return printJSON(v)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(getCmd)
}
