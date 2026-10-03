package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"mrsydar/tagona/storage/pkg/client"
)

func main() {
	slog.Debug("client starting")
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	baseURL, token, remaining := parseGlobalFlags(os.Args[1:])
	if baseURL == "" {
		fmt.Fprintf(os.Stderr, "error: --url is required\n")
		usage()
		os.Exit(1)
	}
	if token == "" {
		token = os.Getenv("API_TOKEN")
	}
	if token == "" {
		fmt.Fprintf(os.Stderr, "api key required: pass --token or set API_TOKEN\n")
		os.Exit(1)
	}
	if len(remaining) < 1 {
		usage()
		os.Exit(1)
	}

	c := client.NewWithToken(baseURL, token)
	ctx := context.Background()

	slog.Debug("executing command", "command", remaining[0])
	switch remaining[0] {
	case "list-collections":
		listCollections(ctx, c, remaining[1:])
	case "create-collection":
		createCollection(ctx, c, remaining[1:])
	case "collection-tags":
		collectionTags(ctx, c, remaining[1:])
	case "upload":
		upload(ctx, c, remaining[1:])
	case "get":
		get(ctx, c, remaining[1:])
	case "data":
		getData(ctx, c, remaining[1:])
	case "tags":
		getTags(ctx, c, remaining[1:])
	case "query":
		query(ctx, c, remaining[1:])
	case "delete":
		deleteObject(ctx, c, remaining[1:])
	case "delete-collection":
		deleteCollection(ctx, c, remaining[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", remaining[0])
		usage()
		os.Exit(1)
	}
}

func usage() {
	slog.Debug("printing usage")
	fmt.Fprintf(os.Stderr, `usage: client --url <service-url> [--token <api-key>] <command> [options]

The API key is required for every request. Pass it with --token or the API_TOKEN
env var; it is sent as "Authorization: Bearer <api-key>".

commands:
  list-collections   List all collections.
  create-collection  --name <name> --data-type <type>
                     Create a new collection with the given name and data type.
  delete-collection  --collection <c>
                     Delete a collection and all its objects.
  collection-tags    --collection <c> [--prefix <p>] [--limit <n>] [--cursor <cursor>]
                     Show the total number of objects in a collection and the tags registered in it,
                     with how many objects each tag is true, false and not yet evaluated (unknown) for.
                     Tags are ordered by name; pass the returned "next" cursor to fetch the next page.
  upload             --collection <c> --data-type <type> --file <path> [--date <RFC3339>] [--ttl <seconds>]
                     Upload an object to a collection.
                     Use --file - to read from stdin.
  get                --collection <c> --id <id>
                     Get object metadata.
  data               --collection <c> --id <id> [--out <path>]
                     Download object data. Default output is stdout, use --out <path> to save to a file.
  tags               --collection <c> --id <id> [--tags <a,b>] [--evaluate=false]
                     Get or evaluate tags for an object.
                     Without --tags, returns all tags for the object.
                     With --tags, evaluates only the given tag names.
                     With --evaluate=false nothing is evaluated: requested tags that are
                     not yet known are returned as null.
  query              --collection <c> [--tag <name=value>] [--tag <name=value>] [--limit <n>] [--cursor <cursor>] [--timeout <ms>] [--best-effort] [--evaluate=false]
                     Query objects in a collection by tag criteria.
                     Use --tag multiple times: --tag \"working with kids=true\" --tag golang=false
                     If no =value is given, the tag defaults to true.
                     With --evaluate=false the tagger is not called: only objects whose requested
                     tags are already known and match are returned.
  delete             --collection <c> --id <id>
                     Delete an object.
`)
}

func parseGlobalFlags(args []string) (string, string, []string) {
	slog.Debug("parsing global flags")
	var url string
	var token string
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--url" {
			if i+1 < len(args) {
				url = args[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(args[i], "--url=") {
			url = strings.TrimPrefix(args[i], "--url=")
			continue
		}
		if args[i] == "--token" {
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(args[i], "--token=") {
			token = strings.TrimPrefix(args[i], "--token=")
			continue
		}
		out = append(out, args[i])
	}
	return url, token, out
}

func createCollection(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("createCollection called")
	fs := flag.NewFlagSet("create-collection", flag.ExitOnError)
	name := fs.String("name", "", "collection name")
	dataType := fs.String("data-type", "", "data type")
	fs.Parse(args)
	if *name == "" || *dataType == "" {
		fs.Usage()
		os.Exit(1)
	}
	coll, err := c.CreateCollection(ctx, *name, *dataType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(coll)
}

func listCollections(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("listCollections called")
	colls, err := c.ListCollections(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(colls)
}

func collectionTags(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("collectionTags called")
	fs := flag.NewFlagSet("collection-tags", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	prefix := fs.String("prefix", "", "only tags starting with this prefix")
	limit := fs.Int("limit", 0, "maximum number of tags to return (server default if 0)")
	cursor := fs.String("cursor", "", "pagination cursor from a previous response")
	fs.Parse(args)
	if *collection == "" {
		fs.Usage()
		os.Exit(1)
	}
	result, err := c.ListCollectionTags(ctx, *collection, client.CollectionTagsOptions{
		Prefix: *prefix,
		Limit:  *limit,
		Cursor: *cursor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(result)
}

func deleteCollection(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("deleteCollection called")
	fs := flag.NewFlagSet("delete-collection", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	fs.Parse(args)
	if *collection == "" {
		fs.Usage()
		os.Exit(1)
	}
	if err := c.DeleteCollection(ctx, *collection); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("deleted")
}

func upload(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("upload called")
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	dataType := fs.String("data-type", "", "data type")
	file := fs.String("file", "", "file to upload (use - for stdin)")
	dateStr := fs.String("date", "", "object date (RFC3339)")
	ttl := fs.Int("ttl", 0, "TTL in seconds")
	fs.Parse(args)
	if *collection == "" || *dataType == "" || *file == "" {
		fs.Usage()
		os.Exit(1)
	}
	var data []byte
	var err error
	if *file == "-" {
		data, err = readStdin()
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading file: %v\n", err)
		os.Exit(1)
	}
	var t time.Time
	if *dateStr != "" {
		t, err = time.Parse(time.RFC3339, *dateStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error parsing date: %v\n", err)
			os.Exit(1)
		}
	}
	resp, err := c.UploadObject(ctx, *collection, *dataType, data, t, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(resp)
}

func get(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("get called")
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	id := fs.String("id", "", "object id")
	fs.Parse(args)
	if *collection == "" || *id == "" {
		fs.Usage()
		os.Exit(1)
	}
	obj, err := c.GetObjectMetadata(ctx, *collection, *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(obj)
}

func getData(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("getData called")
	fs := flag.NewFlagSet("data", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	id := fs.String("id", "", "object id")
	out := fs.String("out", "-", "output file (default stdout)")
	fs.Parse(args)
	if *collection == "" || *id == "" {
		fs.Usage()
		os.Exit(1)
	}
	data, err := c.GetObjectData(ctx, *collection, *id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if *out == "-" {
		os.Stdout.Write(data)
	} else {
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing file: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(*out)
	}
}

func getTags(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("getTags called")
	fs := flag.NewFlagSet("tags", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	id := fs.String("id", "", "object id")
	tags := fs.String("tags", "", "comma-separated tag names")
	evaluate := fs.Bool("evaluate", true, "evaluate requested tags that are not yet known; false returns null for them")
	fs.Parse(args)
	if *collection == "" || *id == "" {
		fs.Usage()
		os.Exit(1)
	}
	var tagList []string
	if *tags != "" {
		tagList = strings.Split(*tags, ",")
	}
	resp, err := c.GetObjectTagsWithOptions(ctx, *collection, *id, client.GetObjectTagsOptions{Tags: tagList, Evaluate: evaluate})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(resp)
}

// tagList implements flag.Value to accept multiple --tag flags.
type tagList []string

func (t *tagList) String() string { return strings.Join(*t, ",") }
func (t *tagList) Set(v string) error {
	*t = append(*t, v)
	return nil
}

// parseTagValue converts "name=value" into a map entry.
// If no "=" is present, the tag defaults to true.
func parseTagValue(s string) (string, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, fmt.Errorf("tag cannot be empty")
	}
	parts := strings.SplitN(s, "=", 2)
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return "", false, fmt.Errorf("tag name cannot be empty")
	}
	if len(parts) == 1 {
		return name, true, nil
	}
	val := strings.TrimSpace(parts[1])
	switch strings.ToLower(val) {
	case "true":
		return name, true, nil
	case "false":
		return name, false, nil
	default:
		return "", false, fmt.Errorf("invalid tag value %q: must be true or false", val)
	}
}

func query(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("query called")
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	var tags tagList
	fs.Var(&tags, "tag", "tag filter in 'name=value' format (can be used multiple times, e.g. --tag \"working with kids=true\" --tag golang=false)")
	limit := fs.Int("limit", 100, "result limit")
	cursor := fs.String("cursor", "", "pagination cursor")
	timeout := fs.Int("timeout", 30000, "query timeout in milliseconds")
	bestEffort := fs.Bool("best-effort", false, "return partial results on timeout")
	evaluate := fs.Bool("evaluate", true, "evaluate tags that are not yet known; false answers from already-known tags only")
	fs.Parse(args)
	if *collection == "" {
		fs.Usage()
		os.Exit(1)
	}
	req := client.TagsQueryRequest{Limit: *limit, Cursor: *cursor, TimeoutMs: *timeout, BestEffort: *bestEffort}
	if !*evaluate {
		req.Evaluate = evaluate
	}
	if len(tags) > 0 {
		req.Tags = make(map[string]bool)
		for _, t := range tags {
			name, val, err := parseTagValue(t)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error parsing tag %q: %v\n", t, err)
				os.Exit(1)
			}
			req.Tags[name] = val
		}
	}
	resp, err := c.QueryObjects(ctx, *collection, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printJSON(resp)
}

func deleteObject(ctx context.Context, c *client.Client, args []string) {
	slog.Debug("deleteObject called")
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	collection := fs.String("collection", "", "collection name")
	id := fs.String("id", "", "object id")
	fs.Parse(args)
	if *collection == "" || *id == "" {
		fs.Usage()
		os.Exit(1)
	}
	if err := c.DeleteObject(ctx, *collection, *id); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("deleted")
}

func printJSON(v any) {
	slog.Debug("printJSON called")
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func readStdin() ([]byte, error) {
	slog.Debug("readStdin called")
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
	}
	return []byte(b.String()), nil
}
