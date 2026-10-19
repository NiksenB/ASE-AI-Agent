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
	maxIterations = 10
	maxInputTokens  = 150000
	maxOutputTokens = 15000
)

var ReadFileInputSchema = GenerateSchema[ReadFileInput]()
var ListFilesInputSchema = GenerateSchema[ListFilesInput]()
var EditFileInputSchema = GenerateSchema[EditFileInput]()

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

func main() {
	client := anthropic.NewClient()

	scanner := bufio.NewScanner(os.Stdin)
	getUserMessage := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		return scanner.Text(), true
	}

	tools := []ToolDefinition{ReadFileDefinition, ListFilesDefinition, EditFileDefinition, SetTodosDefinition, CompleteTodoDefinition}
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
			fmt.Print("You: ")
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

		message, err := a.runInference(ctx, conversation)
		if err != nil {
			return err
		}

		iterations++
		inTokens += message.Usage.InputTokens
		outTokens += message.Usage.OutputTokens

		conversation = append(conversation, message.ToParam())
		truncated := message.StopReason == anthropic.StopReasonMaxTokens

		toolResults := []anthropic.ContentBlockParamUnion{}
		for _, content := range message.Content {
			switch content.Type {
			case "text":
				fmt.Printf("Claude: %s\n", content.Text)
			case "tool_use":
				result := a.executeTool(content.ID, content.Name, content.Input)
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
			fmt.Printf("\n[Stopped: %d calls, %d in / %d token limit exeeded agent loop. Send a message to continue.]\n",
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
		return anthropic.NewToolResultBlock(id, err.Error(), true)
	}
	return anthropic.NewToolResultBlock(id, response, false)
}

func (a *Agent) runInference(ctx context.Context, conversation []anthropic.MessageParam) (*anthropic.Message, error) {
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

	message, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet5_5,
		MaxTokens: int64(4096), //Burde måske sættes op
		Messages:  conversation,
		Tools:     anthropicTools,
		System: []anthropic.TextBlockParam{{
			Text: "For any task, first call set_todos with the goals. Do the work, and call complete_todo for each goal only after verifying it. Do not give a final answer until all goals are complete.",
		}},
	})
	return message, err
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