package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"

	"reasonix/internal/i18n"

	"github.com/spf13/pflag"
)

type commandFlagSet interface {
	Output() io.Writer
	Parse([]string) error
	SetOutput(io.Writer)
}

// parseCommandFlags gives standard flag and pflag commands the same public
// behavior: help is successful, while malformed input prints one concise error
// and returns the conventional command-line usage exit code.
func parseCommandFlags(fs commandFlagSet, args []string) (exitCode int, proceed bool) {
	output := fs.Output()
	var parseOutput bytes.Buffer
	fs.SetOutput(&parseOutput)
	err := fs.Parse(args)
	fs.SetOutput(output)
	if err == nil {
		return 0, true
	}
	if errors.Is(err, flag.ErrHelp) || errors.Is(err, pflag.ErrHelp) {
		_, _ = io.Copy(output, &parseOutput)
		return 0, false
	}
	fmt.Fprintln(output, i18n.M.ErrorPrefix, err)
	return 2, false
}
