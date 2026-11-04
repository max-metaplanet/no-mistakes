package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runorigin"
	"github.com/spf13/cobra"
)

const originPushPrefix = "no-mistakes.caller-origin="

func acceptSelectedRunOrigin(d *db.DB, runID string, requested *runorigin.Submission) error {
	if requested == nil {
		return nil
	}
	run, err := d.GetRun(runID)
	if err != nil {
		return fmt.Errorf("read selected run caller origin: %w", err)
	}
	if run == nil || run.CallerOrigin == nil {
		return fmt.Errorf("accepted run has no caller origin association; refusing unattributed execution")
	}
	if run.CallerOrigin.Submission != *requested {
		return fmt.Errorf("caller origin conflicts with the selected run %s", runID)
	}
	return nil
}

func callerOriginFromFlags(cmd *cobra.Command) (*runorigin.Submission, error) {
	if !cmd.Flags().Changed("caller-origin-file") {
		return nil, nil
	}
	path, _ := cmd.Flags().GetString("caller-origin-file")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read caller origin: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
	if err != nil {
		return nil, err
	}
	caller, err := runorigin.Decode(data)
	if err != nil {
		return nil, err
	}
	top, err := git.Run(cmd.Context(), ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil {
		return nil, err
	}
	return &runorigin.Submission{SourceCheckout: top, Caller: *caller}, nil
}

func formatCallerOriginPushOptions(origin *runorigin.Submission) []string {
	if origin == nil {
		return nil
	}
	data, _ := json.Marshal(origin)
	return []string{originPushPrefix + base64.RawURLEncoding.EncodeToString(data)}
}

func parseCallerOriginPushOptions(options []string) (*runorigin.Submission, error) {
	var origin *runorigin.Submission
	for _, option := range options {
		value, ok := strings.CutPrefix(option, originPushPrefix)
		if !ok {
			continue
		}
		if origin != nil {
			return nil, fmt.Errorf("multiple caller origins are not allowed")
		}
		if len(value) > 32*1024 {
			return nil, fmt.Errorf("caller origin push option exceeds limit")
		}
		data, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid caller origin encoding")
		}
		var s runorigin.Submission
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("invalid caller origin: %w", err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("caller origin must be one JSON object")
		}
		if err := s.Validate(); err != nil {
			return nil, err
		}
		origin = &s
	}
	return origin, nil
}

func requireDaemonHonorsOrigin(client *ipc.Client) error {
	var result ipc.GetExecutionOriginResult
	if err := client.Call(ipc.MethodGetExecutionOrigin, &ipc.GetExecutionOriginParams{}, &result); err != nil {
		return fmt.Errorf("daemon does not support caller origin protocol v1: %w", err)
	}
	if result.ProtocolVersion != 1 {
		return fmt.Errorf("unsupported caller origin protocol version %d", result.ProtocolVersion)
	}
	return nil
}

func newAxiExecutionOriginCmd() *cobra.Command {
	var ref, socket, checkout string
	cmd := &cobra.Command{Use: "execution-origin", Short: "Resolve a producer reference for this exact execution checkout as JSON", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ref == "" {
				ref = os.Getenv(runorigin.ReferenceEnv)
			}
			if socket == "" {
				socket = os.Getenv(runorigin.SocketEnv)
			}
			if ref == "" && socket == "" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(ipc.GetExecutionOriginResult{ProtocolVersion: 1})
			}
			if ref == "" || socket == "" {
				return fmt.Errorf("execution reference and owning socket must both be present")
			}
			if err := runorigin.Reference(ref).Validate(); err != nil {
				return err
			}
			if checkout == "" {
				var err error
				checkout, err = git.Run(cmd.Context(), ".", "rev-parse", "--show-toplevel")
				if err != nil {
					return err
				}
			}
			checkout, err := filepath.Abs(checkout)
			if err != nil {
				return err
			}
			client, err := ipc.Dial(socket)
			if err != nil {
				return fmt.Errorf("connect to execution owner: %w", err)
			}
			defer client.Close()
			var result ipc.GetExecutionOriginResult
			if err := client.Call(ipc.MethodGetExecutionOrigin, &ipc.GetExecutionOriginParams{Reference: runorigin.Reference(ref), ExecutionCheckout: checkout}, &result); err != nil {
				return err
			}
			if result.ProtocolVersion != 1 || result.Execution == nil {
				return fmt.Errorf("execution owner returned no v1 association")
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
	cmd.Flags().StringVar(&ref, "ref", "", "opaque execution reference (default producer environment)")
	cmd.Flags().StringVar(&socket, "socket", "", "owning daemon socket (default producer environment)")
	cmd.Flags().StringVar(&checkout, "checkout", "", "exact execution checkout (default current directory)")
	return cmd
}
