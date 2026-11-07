# Every decision you made and why, everything that surprised you, everything that failed. Short entries, written as they happen

## 19 oct: AI Agent Loop
As descriped in this [blogpost](https://www.requesty.ai/blog/loop-engineering-how-to-build-ai-agent-loops-that-run-themselves) there can be several AI agent loop failures. One we were very concerned with given the 20$ token limit was **token runaway**. Thus, we ensured that over a certain, quite small, amount of used token in a tool loop it would stop and go back to the user to be told wether it could continue or not. 

The code for this was implemented using Claude Sonnet 5.5. 

## 19 oct: Task Verification
To increase confidence that a task has succsesfully been completed a local in memory todo list is made as the first ting the model does. And each time it finnishes a sub task it will attempt a verify and if it goes through it will mark it as complete. If it fails it will send the model back attempt the task again. It was seen how this worked when a bug (described in session "Create agent loop that evaluates output") made it so that a goal could not be marked complete. 

An "in memory" todo list was picked over having e.g. a todo.md as it was deemed fitting for this project of creating a simple AI agent as task todolists likely would never be of a size that would become a problem.

The code for this was implemented using Claude Sonnet 5.5. 

## 21 oct: Shell execution
It is a requirement for the agent to be able to run code in the shell, but the exact commands available varies by execution environment. How to ensure the Agent has all necessary information, without wasting resources with additional 'roundtrips' when the Agent determines that it wants to execute something? For now, the runtime is determined for all Agent clients, though it is sometimes redundant, but no additional roundtrips are made between the user and the AI. 

## 21 oct: Conversation compacting
The context window of an AI is limited but currently our agent endlessly appends to the conversation. Claude automatically compacts at 1m tokens (for Sonnet 5, which is the one we use) as per this [blogpost](https://www.ai-toolbox.co/claude-management-and-productivity/claude-compacting-conversation-2026). However, even when a conversation hits this threshold it still sends the entire conversation to calude, meaning that it will have to compact the conversation every time. The blogpost states that the way they compact is by having the model make a summary, we intend to do the same. The token window will be made lower, mostly so that it is possible to test this function fairly easily. The compact check is after all tools in a request has been executed such that we don't compact in the middle of a task. A test was done with an extremly small compactThreshold (5k tokens) and prompt *"create a client server setup in multiple python files that play tictactoe. Create a testing suite for this and verify that everything works."*. It handled this well. It would often go back in to look at files to see where it got to with commands such as ```cd tictactoe && grep -n \"def close\" -A 14 server.py```. During this test the token limit was also hit multiple times and it could continue just fine with a "continue" prompt. The result was a 4 python scripts (client.py, game.py, protocol.py, server.py) and three unittest files (test_game.py, test_integration.py, test_protocol.py).
The compactThreshold has been raised to 200k, which is on par with Haiku 4.5. This threshold was chosen as it is more realistic, but also on the lower side so that it is easier to trigger such that we as developers can see it working correctly together with other tools.

## 5 nov: Track Output Provenance
The idea of using Git to track AI Outputs came from a class discussion. We thus integrate the AI Agent with git version control / Github to precicely track AI-generated code line-by-line. AI changes will automatically be committed. We decided to document the prompt in the commit, so for any line of code it should be possible to see all prompts / AI Agent modifications that have ever touched that line of code. We assume no business secrets are pasted into AI Agent prompts (as these go straight into the git commit bodies). This works since git commits should be quite easy to query, and very exact in terms of lines of code changed. We further decided that tracking changes should not be limited to tools used by the AI (e.g. NewFile / EditFile tool usage), but also any code executing by the model which modified file contents. This can be tracked for any version-controlled repository. We finally decided to compute an estimated cost and put that into the commit too, since it is convenient to document.

## 7 nov: Using the Git Output Provenance
A python script was created that can get all instances of AI created code by a specific model in a specific time frame. These parameters can be set both as flags or in an interactive menu. The output is saved to either the terminal or a markdown file (depending on parameter). The output will contain:
- The model and time period 
- The file and path
- The line number and diff hunks
- The original prompt that created the change

If an code line has been completly overwrittien in a later commit it will not be in the output. An example of the output has been provided (ai-report-claude-sonnet-5-5-2026-11-07.md). 