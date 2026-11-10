# Contributing to AgentFixture

Thank you for your interest in contributing to **AgentFixture**. Bug reports, RFC proposals, and pull requests are welcome.

## Development Workflow

### Prerequisites
- **Go 1.23+**
- Git

### Setup
1. Fork the repository.
2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR_USERNAME/AgentFixture.git
   cd AgentFixture
   ```
3. Create a branch:
   ```bash
   git checkout -b feat/your-feature
   ```

### Running Tests, Benchmarks & Verification
```bash
go vet ./...
go test -v -count=1 ./...
go test -bench=. -benchmem ./pkg/cassette ./pkg/matcher
gofmt -s -w .
```

### Pull Requests
- Keep pull requests focused and atomic.
- Maintain zero external runtime dependencies beyond `gopkg.in/yaml.v3`.
- Ensure all CI checks pass across Linux, Windows, and macOS.
- Fill out the PR template.
