```terminaloutput
"TELEGRAM_API_KEY=your_api_key" >> .env
"OPEN_AI_API_KEY=open_ai_key" >> .env
"PGPASSWORD=67676786786786" >> .env

docker compose --profile local build
docker compose --profile local up
docker exec -it ollama ollama pull gemma3n:e2b
docker exec -it ollama ollama pull qwen3.5:4b
docker exec -it ollama ollama pull gemma3:4b
docker exec -it ollama ollama pull rnj-1
```