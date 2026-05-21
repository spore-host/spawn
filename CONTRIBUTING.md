# Contributing to spawn

Thank you for your interest in contributing to spawn! This document provides guidelines and instructions for contributing.

## Code of Conduct

Be respectful, professional, and constructive. We're all here to build useful software.

## Ways to Contribute

### Reporting Issues

**Bug reports:**
- Check existing issues first
- Provide minimal reproduction steps
- Include spawn version, OS, Go version
- Include relevant error messages and logs

**Feature requests:**
- Describe the problem you're trying to solve
- Explain why existing features don't work
- Provide use cases and examples

**Template:**
```markdown
### Description
[Clear description of the issue]

### Steps to Reproduce
1. Run `spawn launch --instance-type c7i.xlarge`
2. Wait 5 minutes
3. Run `spawn status <instance-id>`

### Expected Behavior
[What should happen]

### Actual Behavior
[What actually happens]

### Environment
- spawn version: v0.13.1
- OS: macOS 13.5 / Ubuntu 22.04 / etc.
- Go version: 1.21.5
- AWS region: us-east-1

### Additional Context
[Logs, screenshots, etc.]
```

### Documentation

Documentation improvements are always welcome:
- Fix typos, grammar, broken links
- Clarify confusing sections
- Add examples
- Improve diagrams

**Documentation standards:**
- Use active voice ("Run the command" not "The command is run")
- Keep examples realistic and runnable
- Include both simple and complex examples
- Explain the "why" not just the "what"

### Code Contributions

**What we're looking for:**
- Bug fixes
- Performance improvements
- New features (discuss in issue first)
- Test coverage improvements
- Code quality improvements

## Development Setup

### Prerequisites

**Required:**
- Go 1.21+ ([install](https://golang.org/doc/install))
- AWS account with credentials configured
- Git

**Recommended:**
- `make` (for convenience scripts)
- `golangci-lint` ([install](https://golangci-lint.run/usage/install/))
- `staticcheck` ([install](https://staticcheck.io/docs/getting-started/))

### Clone and Build

```bash
# Clone repository
git clone https://github.com/spore-host/spore-host.git
cd spore-host/spawn

# Install dependencies
go mod download

# Build
make build

# Or manually:
go build -o bin/spawn ./cmd/spawn

# Verify
./bin/spawn --version
```

### Running Tests

```bash
# Run all tests
make test

# Run specific package tests
go test ./pkg/aws/...

# Run with coverage
go test -cover ./...

# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Run only short tests (skip integration)
go test -short ./...
```

### Code Quality Checks

**Before committing:**
```bash
# Format code
go fmt ./...

# Run linters
golangci-lint run

# Run static analysis
staticcheck ./...

# Run all checks
make check
```

**Or use pre-commit hook:**
```bash
# Install pre-commit hook
cp scripts/pre-commit .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit

# Now runs automatically on commit
```

## Project Structure

```
spawn/
├── cmd/
│   ├── spawn/           # Main CLI entry point
│   │   └── main.go
│   └── spored/          # Agent daemon
│       └── main.go
├── pkg/
│   ├── aws/             # AWS client wrapper
│   ├── agent/           # spored agent logic
│   ├── params/          # Parameter sweep parsing
│   ├── queue/           # Batch queue management
│   ├── userdata/        # User data template generation
│   └── security/        # Security utilities
├── docs/
│   ├── tutorials/       # Learning-oriented docs
│   ├── how-to/          # Task-oriented docs
│   ├── reference/       # Information-oriented docs
│   └── explanation/     # Understanding-oriented docs
├── scripts/             # Build and utility scripts
└── tests/
    ├── integration/     # Integration tests
    └── e2e/             # End-to-end tests
```

## Coding Standards

### Go Style

**Follow standard Go conventions:**
- [Effective Go](https://golang.org/doc/effective_go.html)
- [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)

**spawn-specific:**
- Use short, idiomatic names (`r` for reader, `ctx` for context, `err` for error)
- Wrap errors with context: `fmt.Errorf("operation: %w", err)`
- Return early on errors (avoid deep nesting)
- Prefer standard library over dependencies
- Group imports: stdlib, external, internal

### Example Code

**Good:**
```go
func LaunchInstance(ctx context.Context, cfg *Config) (*Instance, error) {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create AWS client: %w", err)
	}

	input := buildRunInstancesInput(cfg)
	result, err := client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("run instances: %w", err)
	}

	instance := &Instance{
		ID:         *result.Instances[0].InstanceId,
		Type:       cfg.InstanceType,
		LaunchTime: time.Now(),
	}

	return instance, nil
}
```

**Bad:**
```go
func LaunchInstance(ctx context.Context, cfg *Config) (*Instance, error) {
	client, err := aws.NewClient(ctx)
	if err == nil {  // Don't invert logic
		input := buildRunInstancesInput(cfg)
		result, err := client.RunInstances(ctx, input)
		if err == nil {  // Deep nesting
			instance := &Instance{
				ID:         *result.Instances[0].InstanceId,  // Can panic
				Type:       cfg.InstanceType,
				LaunchTime: time.Now(),
			}
			return instance, nil
		} else {
			return nil, err  // Lost context
		}
	} else {
		return nil, err  // Lost context
	}
}
```

### Testing Standards

**Minimum coverage:** 60% (target 80%+)

**Test structure:**
```go
func TestLaunchInstance(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		want    string
		wantErr bool
	}{
		{
			name: "valid config",
			config: &Config{
				InstanceType: "t3.micro",
				Region:       "us-east-1",
			},
			want:    "i-0abc123",
			wantErr: false,
		},
		{
			name: "invalid region",
			config: &Config{
				InstanceType: "t3.micro",
				Region:       "invalid",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LaunchInstance(context.Background(), tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("LaunchInstance() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != nil && got.ID != tt.want {
				t.Errorf("LaunchInstance() ID = %v, want %v", got.ID, tt.want)
			}
		})
	}
}
```

**Use testdata/ for fixtures:**
```go
func TestParseParamFile(t *testing.T) {
	data, err := os.ReadFile("testdata/params.yaml")
	if err != nil {
		t.Fatal(err)
	}

	params, err := ParseParamFile(data)
	// ...
}
```

### Error Handling

**Wrap errors with context:**
```go
result, err := client.RunInstances(ctx, input)
if err != nil {
	return fmt.Errorf("run instances: %w", err)
}
```

**Don't ignore errors:**
```go
// Bad
json.Unmarshal(data, &config)

// Good
if err := json.Unmarshal(data, &config); err != nil {
	return fmt.Errorf("unmarshal config: %w", err)
}
```

### Security

**Never log secrets:**
```go
// Bad
log.Printf("Launching with API key: %s", apiKey)

// Good
log.Printf("Launching with API key: ****%s", apiKey[len(apiKey)-4:])
```

**Validate all external input:**
```go
func SetTTL(ttlStr string) error {
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return fmt.Errorf("invalid TTL format: %w", err)
	}
	if ttl < 0 {
		return fmt.Errorf("TTL must be positive")
	}
	if ttl > 168*time.Hour {  // 7 days
		return fmt.Errorf("TTL exceeds maximum (7 days)")
	}
	// ...
}
```

## Pull Request Process

### Before Submitting

1. **Create an issue first** (for features and large changes)
2. **Fork the repository** and create a branch
3. **Make your changes** following coding standards
4. **Add tests** for new functionality
5. **Run all checks:** `make check && make test`
6. **Update documentation** if applicable
7. **Commit with conventional commits** (see below)

### Commit Message Format

**Use conventional commits:**
```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `refactor`: Code refactoring
- `test`: Adding tests
- `docs`: Documentation changes
- `chore`: Maintenance tasks

**Examples:**
```
feat(launch): add support for IMDSv2 configuration

Add --metadata-options flag to spawn launch command to configure
instance metadata service v2 settings.

Fixes #123
```

```
fix(agent): prevent race condition in TTL check

Use mutex to protect TTL state access from concurrent goroutines.
```

```
docs: add troubleshooting guide for spot interruptions
```

### PR Description Template

```markdown
## Description
[Clear description of the changes]

## Motivation
[Why is this change needed? What problem does it solve?]

## Related Issues
Fixes #123
Relates to #456

## Changes Made
- [ ] Added feature X
- [ ] Updated documentation
- [ ] Added tests

## Testing
[Describe testing performed]

## Checklist
- [ ] Code follows project conventions
- [ ] Tests added/updated
- [ ] Documentation updated
- [ ] All checks passing (make check && make test)
- [ ] Commit messages follow conventional commit format
```

### Review Process

1. **Automated checks run** (lint, test, build)
2. **Maintainers review** code and provide feedback
3. **Address feedback** and push updates
4. **Approval** from at least one maintainer
5. **Merge** (squash and merge for clean history)

### What to Expect

- **Initial response:** Within 3-7 days
- **Review cycles:** 1-3 rounds of feedback
- **Merge time:** Varies by complexity

## Release Process

**For maintainers:**

1. Update CHANGELOG.md
2. Bump version in cmd/spawn/main.go and cmd/spored/main.go
3. Tag release: `git tag v0.14.0`
4. Push tag: `git push origin v0.14.0`
5. GitHub Actions builds and publishes binaries
6. Create GitHub release with notes

## Getting Help

**Questions about contributing?**
- Open a discussion on GitHub
- Ask in #spawn channel (if applicable)
- Email: scott@example.com

**Stuck on something?**
- Check existing issues and PRs
- Read documentation in docs/
- Ask for help in your PR

## Recognition

Contributors are recognized in:
- CHANGELOG.md (for each release)
- GitHub contributors page
- Release notes

Thank you for contributing to spawn!
