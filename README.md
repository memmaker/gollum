## Standalone Usage

Clone the repository:
```
git clone https://github.com/memmaker/gollum.git
```

Navigate to the project directory:
```
cd gollum
```

Install dependencies:
```
go mod download
```

Build the project:
```bash
go build -o gollum
```

Run the application:
```bash
./gollum
```

## Usage as a Dependency

Import the module in your Go project:
```go
require github.com/memmaker/gollum vX.Y.Z

// import in your code
import "github.com/memmaker/gollum"
``` 

Use the features as per your needs, following the package documentation.

Ensure you run:
```bash
go mod tidy
```
to fetch dependencies and update go.mod.
