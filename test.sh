#!/bin/bash
BENCHMARK_OLLAMA_MODEL='gemma4' \
  go test -v -run TestBenchmark_OllamaProvider -count=1 -timeout 30m
