# Tiny Docker

This is a tiny docker runtime written in Go. Not for production use.

## Usage

### Pull image

```bash
go run main.go pull ubuntu:latest
```

### Run container

```bash
go run main.go run ubuntu:latest /bin/bash
```