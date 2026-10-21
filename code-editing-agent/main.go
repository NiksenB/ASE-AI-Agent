package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"os/exec"
	"runtime"

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
	client        *anthropic.Client
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
	maxIterations   = 10
	maxInputTokens  = 150000
	maxOutputTokens = 15000
	compactThreshold = 200000
)

var lastContextTokens int64

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
	Replaces 'old_str' with 'new_str' in the given file. 'old_str' and 'new_str' MUST be different from each other.\n\nIf the file specified with path doesn't exist, it will be created.
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
		Model:     anthropic.ModelClaudeSonnet5_5,
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
		Model:     anthropic.ModelClaudeSonnet5_5,
		MaxTokens: int64(2048),
		Messages:  msgs,
		System:    systemPrompt(), 
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
		panic(err)
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
		panic(err)
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

	oldContent := string(content)
	newContent := strings.Replace(oldContent, editFileInput.OldStr, editFileInput.NewStr, -1)

	if oldContent == newContent && editFileInput.OldStr != "" {
		return "", fmt.Errorf("old_str not found in file")
	}

	err = os.WriteFile(editFileInput.Path, []byte(newContent), 0644)
	if err != nil {
		return "", err
	}

	return "OK", nil
}

func createNewFile(filePath, content string) (string, error) {
	dir := path.Dir(filePath)
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

	return fmt.Sprintf("Successfully created file %s", filePath), nil
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
	if err != nil {
		return string(output), fmt.Errorf("execution failed: %w", err)
	}

	return string(output), nil
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