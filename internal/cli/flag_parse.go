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

// commandFlagError keeps parse output attached to the returned error so pure
// syntax parsers can defer user-facing reporting to their command boundary.
type commandFlagError struct {
	err         error
	output      io.Writer
	parseOutput string
}

func (e *commandFlagError) Error() string { return e.err.Error() }
func (e *commandFlagError) Unwrap() error { return e.err }

func parseCommandFlagSet(fs commandFlagSet, args []string) error {
	output := fs.Output()
	var parseOutput bytes.Buffer
	fs.SetOutput(&parseOutput)
	err := fs.Parse(args)
	fs.SetOutput(output)
	if err == nil {
		return nil
	}
	return &commandFlagError{err: err, output: output, parseOutput: parseOutput.String()}
}

func reportCommandFlagError(err error) (exitCode int, handled bool) {
	var parseErr *commandFlagError
	if !errors.As(err, &parseErr) {
		return 0, false
	}
	if errors.Is(parseErr, flag.ErrHelp) || errors.Is(parseErr, pflag.ErrHelp) {
		_, _ = io.WriteString(parseErr.output, parseErr.parseOutput)
		return 0, true
	}
	fmt.Fprintln(parseErr.output, i18n.M.ErrorPrefix, parseErr.err)
	return 2, true
}

// parseCommandFlags gives standard flag and pflag commands the same public
// behavior: help is successful, while malformed input prints one concise error
// and returns the conventional command-line usage exit code.
func parseCommandFlags(fs commandFlagSet, args []string) (exitCode int, proceed bool) {
	err := parseCommandFlagSet(fs, args)
	if err == nil {
		return 0, true
	}
	if code, ok := reportCommandFlagError(err); ok {
		return code, false
	}
	fmt.Fprintln(fs.Output(), i18n.M.ErrorPrefix, err)
	return 2, false
}
