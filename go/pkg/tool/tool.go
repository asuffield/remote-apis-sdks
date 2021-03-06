// Package tool provides implementation of the debugging related operations
// supported by go/cmd/remotetool package.
package tool

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bazelbuild/remote-apis-sdks/go/pkg/command"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/digest"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/filemetadata"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/outerr"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/rexec"
	"github.com/bazelbuild/remote-apis-sdks/go/pkg/tree"
	"github.com/golang/protobuf/ptypes"

	rc "github.com/bazelbuild/remote-apis-sdks/go/pkg/client"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
)

const (
	stdoutFile = "stdout"
	stderrFile = "stderr"
)

// Client is a remote execution client.
type Client struct {
	GrpcClient *rc.Client
}

// ReexecuteAction reexecutes the action remotely, optionally overriding the
// inputs with the ones provided at the inputRoot path.
func (c *Client) ReexecuteAction(ctx context.Context, actionDigest, inputRoot string, oe outerr.OutErr) error {
	acDg, err := digest.NewFromString(actionDigest)
	if err != nil {
		return err
	}
	actionProto := &repb.Action{}
	if err := c.GrpcClient.ReadProto(ctx, acDg, actionProto); err != nil {
		return err
	}

	commandProto := &repb.Command{}
	cmdDg, err := digest.NewFromProto(actionProto.GetCommandDigest())
	if err != nil {
		return err
	}

	log.Infof("Reading command from action digest..")
	if err := c.GrpcClient.ReadProto(ctx, cmdDg, commandProto); err != nil {
		return err
	}

	fmc := filemetadata.NewNoopCache()
	client := &rexec.Client{
		FileMetadataCache: fmc,
		GrpcClient:        c.GrpcClient,
	}
	_, inputPaths, err := c.getInputTree(ctx, actionProto.GetInputRootDigest())
	if err != nil {
		return err
	}
	if inputRoot == "" {
		curTime := time.Now().Format(time.RFC3339)
		inputRoot = filepath.Join(os.TempDir(), acDg.Hash+"_"+curTime)
		dg, err := digest.NewFromProto(actionProto.GetInputRootDigest())
		if err != nil {
			return err
		}
		log.Infof("Fetching input tree from input root digest %s into %s", dg, inputRoot)
		_, err = c.GrpcClient.DownloadDirectory(ctx, dg, inputRoot, fmc)
		if err != nil {
			return err
		}
	}
	// Construct Command object.
	cmd := commandFromREProto(commandProto)
	cmd.InputSpec.Inputs = inputPaths
	cmd.ExecRoot = inputRoot
	if actionProto.Timeout != nil {
		tm, err := ptypes.Duration(actionProto.Timeout)
		if err != nil {
			return err
		}
		cmd.Timeout = tm
	}
	opt := &command.ExecutionOptions{AcceptCached: false, DownloadOutputs: true}
	res, _ := client.Run(ctx, cmd, opt, oe)
	switch res.Status {
	case command.NonZeroExitResultStatus:
		oe.WriteErr([]byte(fmt.Sprintf("Remote action FAILED with exit code %d.\n", res.ExitCode)))
	case command.TimeoutResultStatus:
		oe.WriteErr([]byte(fmt.Sprintf("Remote action TIMED OUT after %0f seconds.\n", cmd.Timeout.Seconds())))
	case command.InterruptedResultStatus:
		oe.WriteErr([]byte(fmt.Sprintf("Remote execution was interrupted.\n")))
	case command.RemoteErrorResultStatus:
		oe.WriteErr([]byte(fmt.Sprintf("Remote execution error: %v.\n", res.Err)))
	case command.LocalErrorResultStatus:
		oe.WriteErr([]byte(fmt.Sprintf("Local error: %v.\n", res.Err)))
	}

	return res.Err
}

func commandFromREProto(cmdPb *repb.Command) *command.Command {
	cmd := &command.Command{
		InputSpec: &command.InputSpec{
			EnvironmentVariables: make(map[string]string),
		},
		Identifiers: &command.Identifiers{},
		WorkingDir:  cmdPb.WorkingDirectory,
		OutputFiles: cmdPb.OutputFiles,
		OutputDirs:  cmdPb.OutputDirectories,
		Platform:    make(map[string]string),
		Args:        cmdPb.Arguments,
	}

	for _, ev := range cmdPb.EnvironmentVariables {
		cmd.InputSpec.EnvironmentVariables[ev.Name] = ev.Value
	}
	for _, pt := range cmdPb.GetPlatform().GetProperties() {
		cmd.Platform[pt.Name] = pt.Value
	}
	return cmd
}

// DownloadActionResult downloads the action result of the given action digest
// if it exists in the remote cache.
func (c *Client) DownloadActionResult(ctx context.Context, actionDigest, pathPrefix string) error {
	resPb, err := c.getActionResult(ctx, actionDigest)
	if err != nil {
		return err
	}

	log.Infof("Cleaning contents of %v.", pathPrefix)
	os.RemoveAll(pathPrefix)
	os.Mkdir(pathPrefix, 0755)

	log.Infof("Downloading action results of %v to %v.", actionDigest, pathPrefix)
	// We don't really need an in-memory filemetadata cache for debugging operations.
	noopCache := filemetadata.NewNoopCache()
	if err := c.GrpcClient.DownloadActionOutputs(ctx, resPb, pathPrefix, noopCache); err != nil {
		log.Errorf("Failed downloading action outputs: %v.", err)
	}

	// We have not requested for stdout/stderr to be inlined in GetActionResult, so the server
	// should be returning the digest instead of sending raw data.
	outMsgs := map[string]*repb.Digest{
		filepath.Join(pathPrefix, stdoutFile): resPb.StdoutDigest,
		filepath.Join(pathPrefix, stderrFile): resPb.StderrDigest,
	}
	for path, reDg := range outMsgs {
		if reDg == nil {
			continue
		}
		dg := &digest.Digest{
			Hash: reDg.GetHash(),
			Size: reDg.GetSizeBytes(),
		}
		log.Infof("Downloading stdout/stderr to %v.", path)
		bytes, err := c.GrpcClient.ReadBlob(ctx, *dg)
		if err != nil {
			log.Errorf("Unable to read blob for %v with digest %v.", path, dg)
		}
		if err := ioutil.WriteFile(path, bytes, 0644); err != nil {
			log.Errorf("Unable to write output of digest %v to file %v.", dg, path)
		}
	}
	log.Infof("Successfully downloaded results of %v to %v.", actionDigest, pathPrefix)
	return nil
}

// DownloadBlob downloads a blob from the remote cache into the specified path.
// If the path is empty, it writes the contents to stdout instead.
func (c *Client) DownloadBlob(ctx context.Context, blobDigest, path string) (string, error) {
	outputToStdout := false
	if path == "" {
		outputToStdout = true
		// Create a temp file.
		tmpFile, err := ioutil.TempFile(os.TempDir(), "")
		if err != nil {
			return "", err
		}
		if err := tmpFile.Close(); err != nil {
			return "", err
		}
		path = tmpFile.Name()
		defer os.Remove(path)
	}
	dg, err := digest.NewFromString(blobDigest)
	if err != nil {
		return "", err
	}
	log.Infof("Downloading blob of %v to %v.", dg, path)
	if _, err := c.GrpcClient.ReadBlobToFile(ctx, dg, path); err != nil {
		return "", err
	}
	if !outputToStdout {
		return "", nil
	}
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(contents), nil
}

// DownloadDirectory downloads a an input root from the remote cache into the specified path.
func (c *Client) DownloadDirectory(ctx context.Context, rootDigest, path string) error {
	log.Infof("Cleaning contents of %v.", path)
	os.RemoveAll(path)
	os.Mkdir(path, 0755)

	dg, err := digest.NewFromString(rootDigest)
	if err != nil {
		return err
	}
	log.Infof("Downloading input root %v to %v.", dg, path)
	_, err = c.GrpcClient.DownloadDirectory(ctx, dg, path, filemetadata.NewNoopCache())
	return err
}

// ShowAction parses and displays an action with its corresponding command.
func (c *Client) ShowAction(ctx context.Context, actionDigest string) (string, error) {
	var showActionRes bytes.Buffer
	resPb, err := c.getActionResult(ctx, actionDigest)
	if err != nil {
		return "", err
	}

	acDg, err := digest.NewFromString(actionDigest)
	if err != nil {
		return "", err
	}
	actionProto := &repb.Action{}
	if err := c.GrpcClient.ReadProto(ctx, acDg, actionProto); err != nil {
		return "", err
	}

	commandProto := &repb.Command{}
	cmdDg, err := digest.NewFromProto(actionProto.GetCommandDigest())
	if err != nil {
		return "", err
	}
	showActionRes.WriteString("Command\n========\n")
	showActionRes.WriteString(fmt.Sprintf("Command Digest: %v\n", cmdDg))

	log.Infof("Reading command from action digest..")
	if err := c.GrpcClient.ReadProto(ctx, cmdDg, commandProto); err != nil {
		return "", err
	}
	cmdStr := strings.Join(commandProto.GetArguments(), " ")
	showActionRes.WriteString(fmt.Sprintf("\t%v\n", cmdStr))

	log.Infof("Fetching input tree from input root digest..")
	inpTree, _, err := c.getInputTree(ctx, actionProto.GetInputRootDigest())
	if err != nil {
		return "", err
	}
	showActionRes.WriteString("\nInputs\n======\n")
	showActionRes.WriteString(inpTree)

	log.Infof("Fetching output tree from action result..")
	outs, err := c.getOutputs(ctx, resPb)
	if err != nil {
		return "", err
	}
	showActionRes.WriteString("\n")
	showActionRes.WriteString(outs)
	return showActionRes.String(), nil
}

func (c *Client) getOutputs(ctx context.Context, actionRes *repb.ActionResult) (string, error) {
	var res bytes.Buffer
	res.WriteString("Output Files:\n=============\n")
	for _, of := range actionRes.GetOutputFiles() {
		dg, err := digest.NewFromProto(of.GetDigest())
		if err != nil {
			return "", err
		}
		res.WriteString(fmt.Sprintf("%v, digest: %v\n", of.GetPath(), dg))
	}

	res.WriteString("\nOutput Files From Directories:\n=================\n")
	for _, od := range actionRes.GetOutputDirectories() {
		treeDigest := od.GetTreeDigest()
		dg, err := digest.NewFromProto(treeDigest)
		if err != nil {
			return "", err
		}
		outDirTree := &repb.Tree{}
		if err := c.GrpcClient.ReadProto(ctx, dg, outDirTree); err != nil {
			return "", err
		}

		outputs, _, err := c.flattenTree(ctx, outDirTree)
		if err != nil {
			return "", err
		}
		res.WriteString("\n")
		res.WriteString(outputs)
	}
	return res.String(), nil
}

func (c *Client) getInputTree(ctx context.Context, root *repb.Digest) (string, []string, error) {
	var res bytes.Buffer

	dg, err := digest.NewFromProto(root)
	if err != nil {
		return "", nil, err
	}
	res.WriteString(fmt.Sprintf("[Root directory digest: %v]", dg))

	dirs, err := c.GrpcClient.GetDirectoryTree(ctx, root)
	if err != nil {
		return "", nil, err
	}
	if len(dirs) == 0 {
		return "", nil, fmt.Errorf("Empty directories returned by GetTree for %v", dg)
	}
	t := &repb.Tree{
		Root:     dirs[0],
		Children: dirs,
	}
	inputs, paths, err := c.flattenTree(ctx, t)
	if err != nil {
		return "", nil, err
	}
	res.WriteString("\n")
	res.WriteString(inputs)

	return res.String(), paths, nil
}

func (c *Client) flattenTree(ctx context.Context, t *repb.Tree) (string, []string, error) {
	var res bytes.Buffer
	outputs, err := tree.FlattenTree(t, "")
	if err != nil {
		return "", nil, err
	}
	// Sort the values by path.
	paths := make([]string, 0, len(outputs))
	for path := range outputs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		output := outputs[path]
		if output.IsEmptyDirectory {
			res.WriteString(fmt.Sprintf("%v: [Directory digest: %v]\n", path, output.Digest))
		} else if output.SymlinkTarget != "" {
			res.WriteString(fmt.Sprintf("%v: [Symlink digest: %v, Symlink Target: %v]\n", path, output.Digest, output.SymlinkTarget))
		} else {
			res.WriteString(fmt.Sprintf("%v: [File digest: %v]\n", path, output.Digest))
		}
	}
	return res.String(), paths, nil
}

func (c *Client) getActionResult(ctx context.Context, actionDigest string) (*repb.ActionResult, error) {
	acDg, err := digest.NewFromString(actionDigest)
	if err != nil {
		return nil, err
	}
	d := &repb.Digest{
		Hash:      acDg.Hash,
		SizeBytes: acDg.Size,
	}
	resPb, err := c.GrpcClient.CheckActionCache(ctx, d)
	if err != nil {
		return nil, err
	}
	if resPb == nil {
		return nil, fmt.Errorf("action digest %v not found in cache", d)
	}
	return resPb, nil
}
