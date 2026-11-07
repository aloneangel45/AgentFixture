# Contributing to AgentFixture

Thank you for your interest in contributing to **AgentFixture**! We welcome bug reports, feature suggestions, and pull requests.

## Development Workflow

### Prerequisites
- **Go 1.22+**
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

### Running Tests & Verification
```bash
go test -v ./...
go vet ./...
gofmt -s -w .
```

### Pull Requests
- Keep PRs focused.
- Ensure all CI tests pass.
- Fill out the PR template.
