# Every decision you made and why, everything that surprised you, everything that failed. Short entries, written as they happen

## 19 oct: AI Agent Loop
As descriped in this [blogpost](https://www.requesty.ai/blog/loop-engineering-how-to-build-ai-agent-loops-that-run-themselves) there can be several AI agent loop failures. One we were very concerned with given the 20$ token limit was **token runaway**. Thus, we ensured that over a certain, quite small, amount of used token in a tool loop it would stop and go back to the user to be told wether it could continue or not. 

The code for this was implemented using Claude Sonnet 5.5. 

## 19 oct: Task Verification
To increase confidence that a task has succsesfully been completed a local in memory todo list is made as the first ting the model does. And each time it finnishes a sub task it will attempt a verify and if it goes through it will mark it as complete. If it fails it will send the model back attempt the task again. It was seen how this worked when a bug (described in session "Create agent loop that evaluates output") made it so that a goal could not be marked complete. 

An "in memory" todo list was picked over having e.g. a todo.md as it was deemed fitting for this project of creating a simple AI agent as task todolists likely would never be of a size that would become a problem.

The code for this was implemented using Claude Sonnet 5.5. 

## 21 oct: Shell execution
It is a requirement for the agent to be able to run code in the shell, but the exact commands available varies by execution environment. How to ensure the Agent has all necessary information, without wasting resources with additional 'rountrips' when the Agent determines that it wants to execute something? For now, the runtime is determined for all Agent clients, though it is sometimes redundany, but no additional 'rountrips' are made between the user and the AI. 
