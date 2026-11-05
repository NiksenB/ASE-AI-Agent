package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/invopop/jsonschema"
)

type ToolDefinition struct {
	Name        string                         //`json:"name"`
	Description string                         //`json:"description"`
	InputSchema anthropic.ToolInputSchemaParam //`json:"input_schema"`
	Function    func(input json.RawMessage) (string, error)
}

type Agent struct {
	client         *anthropic.Client
	getUserMessage func() (string, bool)
	tools          []ToolDefinition
}

type ReadFileInput struct {
	Path string `json:"path" jsonschema_description:"The relative path of a file in the working directory."`
}

type ListFilesInput struct {
	Path string `json:"path,omitempty" jsonschema_description:"Optional relative path to list files from. Defaults to current directory if not provided."`
}

type EditFileInput struct {
	Path   string `json:"path" jsonschema_description:"The path to the file"`
	OldStr string `json:"old_str" jsonschema_description:"Text to search for - must match exactly and must only have one match exactly"`
	NewStr string `json:"new_str" jsonschema_description:"Text to replace old_str with"`
}

type ExecuteCodeInput struct {
	Code string `json:"code" jsonschema_description:"The code to execute."`
}

type Todo struct {
	Text string
	Done bool
}

var todoList []Todo

type SetTodosInput struct {
	Items []string `json:"items" jsonschema_description:"Ordered list of goals needed to complete the task."`
}

type CompleteTodoInput struct {
	Index int `json:"index" jsonschema_description:"1-based index of the goal that is now done and verified."`
}

const (
	maxIterations    = 10
	maxInputTokens   = 150000
	maxOutputTokens  = 15000
	compactThreshold = 200000
)

// Email used as the git *author* of AI commits. The author name is the exact
// model version, so `git blame` shows which model wrote each line.
// The committer stays the human (from their normal git config).
const aiAuthorEmail = "ai-agent@example.invalid"

//var model = anthropic.ModelClaudeSonnet5_5
var model = anthropic.ModelClaudeHaiku4_5

var lastContextTokens int64

// Provenance context, read by the tools when they commit.
var (
	currentPrompt string // the user's latest prompt (kept even after compaction)
	currentModel  string // exact model version reported by the API for the latest response
	repoRoot      string // git top-level directory
	gitEnabled    bool   // false if the working directory is not inside a git repo
)

var ReadFileInputSchema = GenerateSchema[ReadFileInput]()
var ListFilesInputSchema = GenerateSchema[ListFilesInput]()
var EditFileInputSchema = GenerateSchema[EditFileInput]()
var ExecuteCodeInputSchema = GenerateSchema[ExecuteCodeInput]()

var ReadFileDefinition = ToolDefinition{
	Name:        "read_file",
	Description: "Read the contents of a given relative file path. Use this when you want to see what's inside a file. Do not use this with directory names.",
	InputSchema: ReadFileInputSchema,
	Function:    ReadFile,
}

var ListFilesDefinition = ToolDefinition{
	Name:        "list_files",
	Description: "List files and directories at a given path. If no path is provided, lists files in the current directory.",
	InputSchema: ListFilesInputSchema,
	Function:    ListFiles,
}

var EditFileDefinition = ToolDefinition{
	Name: "edit_file",
	Description: `Make edits to a text file.
	Replaces 'old_str' with 'new_str' in the given file. 'old_str' and 'new_str' MUST be different from each other, and 'old_str' must match exactly one place in the file.
	If the file specified with path doesn't exist, it will be created (set old_str to "" and put the full content in new_str).
	`,
	InputSchema: EditFileInputSchema,
	Function:    EditFile,
}

var SetTodosDefinition = ToolDefinition{
	Name:        "set_todos",
	Description: "Create the list of goals for the current task. Call this FIRST, before doing any work.",
	InputSchema: GenerateSchema[SetTodosInput](),
	Function:    SetTodos,
}

var CompleteTodoDefinition = ToolDefinition{
	Name:        "complete_todo",
	Description: "Mark a goal as done. Only call this after verifying the goal is actually met.",
	InputSchema: GenerateSchema[CompleteTodoInput](),
	Function:    CompleteTodo,
}

var ExecuteCodeDefinition = ToolDefinition{
	Name:        "execute_code",
	Description: "Execute a single shell command line and return its output. The OS and shell are stated in the system prompt. The user is asked to confirm before it runs.",
	InputSchema: GenerateSchema[ExecuteCodeInput](),
	Function:    ExecuteCode,
}

var stdin = bufio.NewScanner(os.Stdin)

func main() {
	client := anthropic.NewClient()

	initGit()

	getUserMessage := func() (string, bool) {
		if !stdin.Scan() {
			return "", false
		}
		return stdin.Text(), true
	}

	tools := []ToolDefinition{ReadFileDefinition, ListFilesDefinition, EditFileDefinition, SetTodosDefinition, CompleteTodoDefinition, ExecuteCodeDefinition}
	agent := NewAgent(&client, getUserMessage, tools)
	err := agent.Run(context.TODO())
	if err != nil {
		fmt.Printf("Error: %s\n", err.Error())
	}
}

func NewAgent(client *anthropic.Client, getUserMessage func() (string, bool), tools []ToolDefinition) *Agent {
	return &Agent{
		client:         client,
		getUserMessage: getUserMessage,
		tools:          tools,
	}
}

func (a *Agent) Run(ctx context.Context) error {
	conversation := []anthropic.MessageParam{}

	fmt.Println("Chat with Claude (use 'ctrl-c' to quit)")

	var iterations int
	var inTokens, outTokens int64
	var verifyAttempts int

	readUserInput := true
	for {
		if readUserInput {
			fmt.Print("\u001b[94mYou\u001b[0m: ")
			userInput, ok := a.getUserMessage()
			if !ok {
				break
			}

			iterations, inTokens, outTokens = 0, 0, 0
			verifyAttempts = 0
			todoList = nil
			currentPrompt = userInput

			userMessage := anthropic.NewUserMessage(anthropic.NewTextBlock(userInput))
			conversation = append(conversation, userMessage)
		}

		if lastContextTokens >= compactThreshold {
			compacted, err := a.compact(ctx, conversation)
			if err != nil {
				fmt.Printf("[compaction failed: %v]\n", err)
			} else {
				conversation = compacted
				fmt.Println("[Conversation compacted]")
			}
			lastContextTokens = 0
		}

		message, err := a.runInference(ctx, conversation)
		if err != nil {
			return err
		}

		// Exact model version that produced this response (not the alias).
		currentModel = string(message.Model)

		iterations++
		inTokens += message.Usage.InputTokens
		outTokens += message.Usage.OutputTokens
		lastContextTokens = message.Usage.InputTokens + message.Usage.OutputTokens

		conversation = append(conversation, message.ToParam())
		truncated := message.StopReason == anthropic.StopReasonMaxTokens

		toolResults := []anthropic.ContentBlockParamUnion{}
		for _, content := range message.Content {
			switch content.Type {
			case "text":
				fmt.Printf("\u001b[93mClaude\u001b[0m: %s\n", content.Text)
			case "tool_use":
				var result anthropic.ContentBlockParamUnion
				if truncated {
					result = anthropic.NewToolResultBlock(content.ID,
						"Your output was cut off by the token limit, so this call was incomplete. Write smaller files or split the work into several calls.", true)
				} else {
					result = a.executeTool(content.ID, content.Name, content.Input)
				}

				toolResults = append(toolResults, result)
			}
		}
		if len(toolResults) == 0 {
			if open := openTodos(); open != "" && verifyAttempts < 3 {
				verifyAttempts++
				fmt.Println("[Open goals remain, sending model back to work]")
				conversation = append(conversation, anthropic.NewUserMessage(
					anthropic.NewTextBlock("These goals are not complete yet:\n"+open+"\nFinish them and call complete_todo, or explain why they can't be done.")))
				readUserInput = false
				continue
			}
			readUserInput = true
			continue
		}

		conversation = append(conversation, anthropic.NewUserMessage(toolResults...))

		if iterations >= maxIterations || inTokens >= maxInputTokens || outTokens >= maxOutputTokens {
			fmt.Printf("\n[Stopped: %d calls, %d in / %d out tokens used. Token limit exeeded agent loop. Send a message to continue.]\n",
				iterations, inTokens, outTokens)
			readUserInput = true
			continue
		}

		readUserInput = false
	}

	return nil
}

func (a *Agent) executeTool(id, name string, input json.RawMessage) anthropic.ContentBlockParamUnion {
	var toolDef ToolDefinition
	var found bool
	for _, tool := range a.tools {
		if tool.Name == name {
			toolDef = tool
			found = true
			break
		}
	}
	if !found {
		return anthropic.NewToolResultBlock(id, "tool not found", true)
	}

	fmt.Printf("\u001b[92mtool\u001b[0m: %s(%s)\n", name, input)
	response, err := toolDef.Function(input)
	if err != nil {
		msg := err.Error()
		if response != "" {
			msg = response + "\n" + msg
		}
		return anthropic.NewToolResultBlock(id, msg, true)
	}
	return anthropic.NewToolResultBlock(id, response, false)
}

func (a *Agent) anthropicTools() []anthropic.ToolUnionParam {
	anthropicTools := []anthropic.ToolUnionParam{}
	for _, tool := range a.tools {
		anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        tool.Name,
				Description: anthropic.String(tool.Description),
				InputSchema: tool.InputSchema,
			},
		})
	}
	return anthropicTools
}

func (a *Agent) runInference(ctx context.Context, conversation []anthropic.MessageParam) (*anthropic.Message, error) {
	message, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: int64(4096), //Burde måske sættes op
		Messages:  conversation,
		Tools:     a.anthropicTools(),
		System:    systemPrompt(),
	})
	return message, err
}

// Placed outside to not create a 400 Bad Request.
// systemPrompts are cryptographically bound to the system prompt they were created under.
// so this created problems for the summary function that is a different prompt (:
func systemPrompt() []anthropic.TextBlockParam {
	return []anthropic.TextBlockParam{{
		Text: "You are running on " + shellDescription() + "\n" +
			"For simple questions or conversation, answer directly. For any multi-step task, first call set_todos with the goals, do the work, and call complete_todo for each goal only after verifying it. Do not give a final answer to a multi-step task until all its goals are complete.",
	}}
}

const summaryPrompt = `Summarize this conversation so work can continue from the summary alone.
Include: the user's goals, what has been done (files changed, commands run, results),
key decisions and facts, and what remains to be done. Be concise but complete.`

func (a *Agent) compact(ctx context.Context, conversation []anthropic.MessageParam) ([]anthropic.MessageParam, error) {
	msgs := append([]anthropic.MessageParam{}, conversation...)
	msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(summaryPrompt)))

	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      anthropic.ModelClaudeSonnet5_5,
		MaxTokens:  int64(2048),
		Messages:   msgs,
		System:     systemPrompt(),
		Tools:      a.anthropicTools(),
		ToolChoice: anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}},
	})
	if err != nil {
		return nil, err
	}

	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return nil, fmt.Errorf("empty summary")
	}

	text := "Summary of the conversation so far:\n\n" + sb.String()
	if t := renderTodos(); t != "" {
		text += "\n\nCurrent goals:\n" + t
	}
	text += "\nContinue from here."

	return []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(text))}, nil
}

func GenerateSchema[T any]() anthropic.ToolInputSchemaParam {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var v T

	schema := reflector.Reflect(v)

	return anthropic.ToolInputSchemaParam{
		Properties: schema.Properties,
		Required:   schema.Required,
	}
}

func ReadFile(input json.RawMessage) (string, error) {
	readFileInput := ReadFileInput{}
	err := json.Unmarshal(input, &readFileInput)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(readFileInput.Path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func ListFiles(input json.RawMessage) (string, error) {
	listFilesInput := ListFilesInput{}
	err := json.Unmarshal(input, &listFilesInput)
	if err != nil {
		return "", err
	}

	dir := "."
	if listFilesInput.Path != "" {
		dir = listFilesInput.Path
	}

	var files []string
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Don't dump git internals into the model's context.
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		if relPath != "." {
			if info.IsDir() {
				files = append(files, relPath+"/")
			} else {
				files = append(files, relPath)
			}
		}
		return nil
	})

	if err != nil {
		return "", err
	}

	result, err := json.Marshal(files)
	if err != nil {
		return "", err
	}

	return string(result), nil
}

func EditFile(input json.RawMessage) (string, error) {
	editFileInput := EditFileInput{}
	err := json.Unmarshal(input, &editFileInput)
	if err != nil {
		return "", err
	}

	if editFileInput.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if editFileInput.OldStr == editFileInput.NewStr {
		return "", fmt.Errorf("old_str and new_str are identical or both empty; to create a file, set old_str to \"\" and put the full content in new_str")
	}

	content, err := os.ReadFile(editFileInput.Path)
	if err != nil {
		if os.IsNotExist(err) && editFileInput.OldStr == "" {
			return createNewFile(editFileInput.Path, editFileInput.NewStr)
		}
		return "", err
	}

	if editFileInput.OldStr == "" {
		return "", fmt.Errorf("file already exists; old_str must be non-empty to edit it")
	}

	oldContent := string(content)
	switch n := strings.Count(oldContent, editFileInput.OldStr); {
	case n == 0:
		return "", fmt.Errorf("old_str not found in file")
	case n > 1:
		return "", fmt.Errorf("old_str matches %d places in the file; it must match exactly one", n)
	}
	newContent := strings.Replace(oldContent, editFileInput.OldStr, editFileInput.NewStr, 1)

	// If the file has uncommitted human changes, commit them as the human first,
	// so they are not attributed to the AI.
	if err := commitHumanChanges(editFileInput.Path); err != nil {
		return "", fmt.Errorf("could not snapshot existing uncommitted changes, edit aborted: %w", err)
	}

	err = os.WriteFile(editFileInput.Path, []byte(newContent), 0644)
	if err != nil {
		return "", err
	}

	return commitResult("OK", editFileInput.Path, "AI edit: "+editFileInput.Path, "", "", "edit_file", "")
}

func createNewFile(filePath, content string) (string, error) {
	dir := filepath.Dir(filePath)
	if dir != "." {
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return "", fmt.Errorf("failed to create directory: %w", err)
		}
	}

	err := os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}

	return commitResult(fmt.Sprintf("Successfully created file %s", filePath),
		filePath, "AI create: "+filePath, "", "", "edit_file", "")
}

// ---------------------------------------------------------------------------
// Git provenance tracking
//
// Every AI change becomes one commit:
//   author    = exact model version (shows up in `git blame`)
//   committer = the human (normal git config)
//   body      = full prompt, then trailers (AI-Model, AI-Tool, AI-Note)
//   diff      = the changed code
//
// Query examples:
//   git blame <file>
//   git log --author="claude-" --format='%h %an %s'
//   git log --grep='some words from a prompt'
//   git log --format='%h %(trailers:key=AI-Model,valueonly)'
// ---------------------------------------------------------------------------

func initGit() {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Println("[warning: not inside a git repository, AI provenance tracking is disabled]")
		return
	}
	repoRoot = strings.TrimSpace(string(out))
	gitEnabled = true
}

// runGit runs git from the repo root. File arguments should be absolute paths.
func runGit(stdinText string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), env...)
	if stdinText != "" {
		cmd.Stdin = strings.NewReader(stdinText)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s failed: %w: %s", args[0], err,
			strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.String(), nil
}

// commitFiles stages and commits exactly the given files (absolute paths).
// --only makes sure anything else the user has staged is left alone.
// Hooks are skipped (--no-verify) because intermediate AI edits may not pass
// linters, and a failing hook would silently lose the provenance record.
func commitFiles(files []string, message string, env []string) error {
	if len(files) == 0 {
		return nil
	}
	if _, err := runGit("", nil, append([]string{"add", "--"}, files...)...); err != nil {
		return err
	}
	staged, err := runGit("", nil, append([]string{"diff", "--cached", "--name-only", "--"}, files...)...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(staged) == "" {
		return nil // nothing actually changed
	}
	args := append([]string{"commit", "--only", "--no-verify", "--cleanup=verbatim", "-F", "-", "--"}, files...)
	_, err = runGit(message, env, args...)
	return err
}

// commitHumanChanges commits existing uncommitted changes to a single file
// under the user's normal identity, before the AI touches it.
func commitHumanChanges(path string) error {
	if !gitEnabled {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	status, err := runGit("", nil, "status", "--porcelain", "--", abs)
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}
	return commitFiles([]string{abs}, "Human edits to "+path+" (before AI edit)\n", nil)
}

// commitResult commits an AI change to one file and returns the tool result.
// A failed commit does not fail the tool (the file was already written); it
// adds a warning instead.
func commitResult(okMsg, path, subject, detailLabel, detail, tool, note string) (string, error) {
	if !gitEnabled {
		return okMsg, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return okMsg + " (warning: change not committed: " + err.Error() + ")", nil
	}
	if err := commitAI([]string{abs}, subject, detailLabel, detail, tool, note); err != nil {
		fmt.Printf("[warning: AI change not committed: %v]\n", err)
		return okMsg + " (warning: change not committed to git: " + err.Error() + ")", nil
	}
	return okMsg, nil
}

func commitAI(files []string, subject, detailLabel, detail, tool, note string) error {
	version := currentModel
	if version == "" {
		version = string(model)
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + version,
		"GIT_AUTHOR_EMAIL=" + aiAuthorEmail,
	}

	var sb strings.Builder
	sb.WriteString(subject)
	if s := promptSummary(currentPrompt); s != "" {
		sb.WriteString(" - " + s)
	}
	sb.WriteString("\n\nPrompt:\n" + indent(currentPrompt) + "\n\n")
	if detail != "" {
		sb.WriteString(detailLabel + ":\n" + indent(detail) + "\n\n")
	}
	// Trailers must be the last paragraph.
	sb.WriteString("AI-Model: " + version + "\n")
	sb.WriteString("AI-Tool: " + tool + "\n")
	if note != "" {
		sb.WriteString("AI-Note: " + note + "\n")
	}

	return commitFiles(files, sb.String(), env)
}

func indent(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// promptSummary returns the first non-empty line, truncated, for the subject.
func promptSummary(prompt string) string {
	return truncate(firstLine(prompt), 50)
}

func firstLine(s string) string {
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// dirtySnapshot maps every changed/untracked file (path relative to repo root)
// to a hash of its current content ("deleted" if missing).
func dirtySnapshot() (map[string]string, error) {
	out, err := runGit("", nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	snap := map[string]string{}
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		status, p := e[:2], e[3:]
		if status[0] == 'R' || status[0] == 'C' {
			i++ // skip the original path of a rename/copy
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, p))
		if err != nil {
			snap[p] = "deleted"
			continue
		}
		sum := sha256.Sum256(data)
		snap[p] = hex.EncodeToString(sum[:])
	}
	return snap, nil
}

func SetTodos(input json.RawMessage) (string, error) {
	var in SetTodosInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	todoList = nil
	for _, item := range in.Items {
		todoList = append(todoList, Todo{Text: item})
	}
	return renderTodos(), nil
}

func CompleteTodo(input json.RawMessage) (string, error) {
	var in CompleteTodoInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Index < 1 || in.Index > len(todoList) {
		return "", fmt.Errorf("index out of range")
	}
	todoList[in.Index-1].Done = true
	return renderTodos(), nil
}

func renderTodos() string {
	var sb strings.Builder
	for i, t := range todoList {
		mark := " "
		if t.Done {
			mark = "x"
		}
		fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, mark, t.Text)
	}
	return sb.String()
}

func openTodos() string {
	var sb strings.Builder
	for i, t := range todoList {
		if !t.Done {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, t.Text)
		}
	}
	return sb.String()
}

func ExecuteCode(input json.RawMessage) (string, error) {
	var in ExecuteCodeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}

	permission := requestPermission(in.Code)

	if !permission {
		return "", fmt.Errorf("execution denied")
	}

	var before map[string]string
	if gitEnabled {
		var err error
		before, err = dirtySnapshot()
		if err != nil {
			fmt.Printf("[warning: could not snapshot git state, command changes will not be tracked: %v]\n", err)
			before = nil
		}
	}

	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		// cmd.exe:  /C = run the command, then exit
		cmd = exec.Command("cmd", "/C", in.Code)

		// PowerShell version:
		// cmd = exec.Command("powershell", "-NoProfile", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", in.Code)
	}

	output, err := cmd.CombinedOutput()
	result := string(output)

	// Commit whatever the command changed, even if it exited non-zero.
	if before != nil {
		if warn := commitShellChanges(in.Code, before); warn != "" {
			fmt.Println(warn)
			result += "\n" + warn
		}
	}

	if err != nil {
		return result, fmt.Errorf("execution failed: %w", err)
	}

	return result, nil
}

// commitShellChanges commits files whose content differs from the snapshot
// taken before the command ran. Returns a warning string on failure.
func commitShellChanges(command string, before map[string]string) string {
	after, err := dirtySnapshot()
	if err != nil {
		return "[warning: could not snapshot git state after command: " + err.Error() + "]"
	}

	var changed, preDirty []string
	for p, h := range after {
		bh, wasDirty := before[p]
		if wasDirty && bh == h {
			continue
		}
		changed = append(changed, p)
		if wasDirty {
			preDirty = append(preDirty, p)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	sort.Strings(changed)
	sort.Strings(preDirty)

	abs := make([]string, len(changed))
	for i, p := range changed {
		abs[i] = filepath.Join(repoRoot, p)
	}

	note := ""
	if len(preDirty) > 0 {
		note = "these files had uncommitted changes before the command, so this commit may include human edits: " + strings.Join(preDirty, ", ")
	}

	subject := "AI shell: " + truncate(firstLine(command), 50)
	detail := command + "\n\nFiles:\n" + strings.Join(changed, "\n")
	if err := commitAI(abs, subject, "Command", detail, "execute_code", note); err != nil {
		return "[warning: shell changes not committed to git: " + err.Error() + "]"
	}
	return ""
}

func requestPermission(code string) bool {
	fmt.Printf("Do you want to execute the following code? (yes/no): \n%s\n", code)
	stdin.Scan()
	response := stdin.Text()
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "y", "yes":
		return true
	}
	return false
}

func shellDescription() string {
	if runtime.GOOS == "windows" {
		return "Windows (cmd.exe). Each call is a fresh shell; cd does not persist."
	}
	return runtime.GOOS + " (sh). Each call is a fresh shell; cd does not persist."
}