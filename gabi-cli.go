package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/app-sre/gabi/pkg/models"
	routev1 "github.com/openshift/api/route/v1"
	routeclientv1 "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	"github.com/elk-language/go-prompt"
	istrings "github.com/elk-language/go-prompt/strings"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

func main() {
	var kubeconfigPath *string

	if home := homedir.HomeDir(); home != "" {
		kubeconfigPath = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfigPath = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	showHelp := flag.Bool("h", false, "Shows help")
	quiet := flag.Bool("q", false, "Suppress logging messages")
	fancy := flag.Bool("fancy", false, "Use rounded table style with colored header")
	display := flag.String("display", "auto", "Display mode: auto, table, or expanded")
	namespace := flag.String("n", "", "Namespace (defaults to current context)")
	flag.Parse()

	if *showHelp {
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *quiet {
		log.SetOutput(ioutil.Discard)
	}
	kubeconfig, config := setupK8s(*kubeconfigPath)
	setDefaultNamespace(kubeconfig, namespace)

	bearerToken := config.BearerToken
	if bearerToken == "" {
		log.Fatalf("no Bearer Token please use `oc login`")
	}

	log.Printf("Looking up Gabi from namespace %s, cluster %s", *namespace, config.Host)
	gabiRoute, err := getGabiRoute(config, *namespace)

	if err != nil {
		if apierrors.IsUnauthorized(err) {
			log.Fatalf("%s, please login with oc login", err)
		} else {
			log.Fatalf("couldn't find Gabi instance: %s", err)
		}
	}

	gabiUrl := gabiUrlFromRoute(gabiRoute)
	log.Printf("Using Gabi %s", gabiUrl)

	historyFile := ""
	if home := homedir.HomeDir(); home != "" {
		historyFile = filepath.Join(home, ".gabi_history")
	}
	var qh *QueryHistory
	if historyFile != "" {
		qh = NewQueryHistory(historyFile)
	}

	displayMode := *display

	var query string
	if len(flag.Args()) > 0 {
		query = strings.Join(flag.Args(), " ")
		var buf bytes.Buffer
		runQuery(gabiUrl, bearerToken, "", &query, qh, *fancy, displayMode, &buf)
		pageOutput(buf.String())
		return
	}

	var historyOpts []prompt.Option
	if qh != nil {
		historyOpts = append(historyOpts, prompt.WithCustomHistory(qh))
	}

	opts := append([]prompt.Option{
		prompt.WithPrefix("gabi> "),
		prompt.WithKeyBind(prompt.KeyBind{
			Key: prompt.ControlO,
			Fn: func(p *prompt.Prompt) bool {
				buf := p.Buffer()
				currentText := buf.Text()
				initial := currentText
				if initial == "" && qh != nil {
					initial = qh.LastQuery()
				}
				edited, err := openEditor(initial)
				if err == errEditorNoSave {
					return true
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "Editor error: %s\n", err)
					return true
				}
				runeLen := istrings.RuneCountInString(currentText)
				if runeLen > 0 {
					p.DeleteBeforeCursorRunes(runeLen)
				}
				p.InsertTextMoveCursor(edited, false)
				return true
			},
		}),
	}, historyOpts...)

	p := prompt.New(func(input string) {
		trimmed := strings.TrimSpace(input)
		if trimmed == `\e` {
			lastQuery := ""
			if qh != nil {
				lastQuery = qh.LastQuery()
			}
			edited, err := openEditor(lastQuery)
			if err == errEditorNoSave {
				return
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Editor error: %s\n", err)
				return
			}
			query = ""
			var buf bytes.Buffer
			runQuery(gabiUrl, bearerToken, edited, &query, qh, *fancy, displayMode, &buf)
			pageOutput(buf.String())
			return
		}
		if trimmed == `\x` {
			if displayMode == "expanded" {
				displayMode = "auto"
				fmt.Println("Expanded display is off.")
			} else {
				displayMode = "expanded"
				fmt.Println("Expanded display is on.")
			}
			return
		}
		if trimmed == `\d` {
			var buf bytes.Buffer
			runBuiltinQuery(gabiUrl, bearerToken, sqlListSchema, &buf, *fancy, displayMode)
			pageOutput(buf.String())
			return
		}
		if strings.HasPrefix(trimmed, `\d `) {
			tableName := strings.TrimSpace(trimmed[3:])
			var buf bytes.Buffer
			describeTable(gabiUrl, bearerToken, tableName, &buf, *fancy, displayMode)
			pageOutput(buf.String())
			return
		}
		if trimmed == `\stats` || strings.HasPrefix(trimmed, `\stats `) {
			var buf bytes.Buffer
			runStats(gabiUrl, bearerToken, trimmed, &buf, *fancy, displayMode)
			pageOutput(buf.String())
			return
		}
		if trimmed == `\h` || trimmed == `\help` || trimmed == `help` {
			printHelp()
			return
		}
		{
			var buf bytes.Buffer
			runQuery(gabiUrl, bearerToken, input, &query, qh, *fancy, displayMode, &buf)
			pageOutput(buf.String())
		}
	}, opts...)
	p.Run()
}

const queryDelimiter = "\n\x00\n"
const maxHistorySize = 500

type QueryHistory struct {
	prompt.History
	path    string
	queries []string
}

func NewQueryHistory(path string) *QueryHistory {
	qh := &QueryHistory{
		History: *prompt.NewHistory(),
		path:    path,
	}
	if data, err := os.ReadFile(path); err == nil {
		content := strings.TrimRight(string(data), "\n")
		if content != "" {
			for _, q := range strings.Split(content, queryDelimiter) {
				q = strings.TrimSpace(q)
				if q != "" {
					qh.queries = append(qh.queries, q)
					qh.History.Add(q)
				}
			}
		}
	}
	return qh
}

func (qh *QueryHistory) AddQuery(q string) {
	qh.queries = append(qh.queries, q)
	if len(qh.queries) > maxHistorySize {
		qh.queries = qh.queries[len(qh.queries)-maxHistorySize:]
	}
	qh.History.Add(q)
	qh.save()
}

func (qh *QueryHistory) save() {
	if err := os.WriteFile(qh.path, []byte(strings.Join(qh.queries, queryDelimiter)+"\n"), 0600); err != nil {
		log.Printf("warning: failed to save history: %s", err)
	}
}

func (qh *QueryHistory) LastQuery() string {
	if len(qh.queries) > 0 {
		return qh.queries[len(qh.queries)-1]
	}
	return ""
}

func getEditor() string {
	if e := os.Getenv("VISUAL"); e != "" {
		return e
	}
	if e := os.Getenv("EDITOR"); e != "" {
		return e
	}
	return "vi"
}

var errEditorNoSave = fmt.Errorf("editor exited without saving")

func openEditor(content string) (string, error) {
	f, err := os.CreateTemp("", "gabi-*.sql")
	if err != nil {
		return "", err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	statBefore, err := os.Stat(tmpPath)
	if err != nil {
		return "", err
	}

	editor := getEditor()
	cmd := exec.Command("sh", "-c", editor+" \"$1\"", "--", tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor %s exited with: %w", editor, err)
	}

	statAfter, err := os.Stat(tmpPath)
	if err != nil {
		return "", err
	}
	if statAfter.ModTime().Equal(statBefore.ModTime()) {
		return "", errEditorNoSave
	}

	result, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(result), "\n"), nil
}

func printHelp() {
	fmt.Print(`Available commands:
  \d              list tables, views, and sequences
  \d <table>      describe table (columns, indexes, references)
  \stats          show all database statistics
  \stats <N>      show statistic N (1=DB sizes, 2=table sizes,
                  3=running queries, 4=index usage, 5=unused indexes)
  \e              open last query in $EDITOR
  \x              toggle expanded display mode
  \h, \help       show this help
  Ctrl+O          open current buffer (or last query) in $EDITOR
  Ctrl+D          exit
`)
}

const sqlListSchema = `SELECT n.nspname AS schema, c.relname AS name,
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view'
    WHEN 'm' THEN 'materialized view' WHEN 'S' THEN 'sequence'
    WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table'
  END AS type,
  pg_catalog.pg_get_userbyid(c.relowner) AS owner
FROM pg_catalog.pg_class c
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','v','m','S','f')
  AND n.nspname <> 'pg_catalog'
  AND n.nspname <> 'information_schema'
  AND n.nspname !~ '^pg_toast'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1, 2;`

const sqlDescribeColumns = `SELECT a.attname AS column,
  pg_catalog.format_type(a.atttypid, a.atttypmod) AS type,
  CASE WHEN a.attnotnull THEN 'not null' ELSE '' END AS nullable,
  (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true)
   FROM pg_catalog.pg_attrdef d
   WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef) AS default
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = '%s'::regclass AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum;`

const sqlDescribeIndexes = `SELECT c2.relname AS name,
  pg_catalog.pg_get_constraintdef(con.oid, true) AS constraint,
  pg_catalog.pg_get_indexdef(i.indexrelid, 0, true) AS definition
FROM pg_catalog.pg_class c, pg_catalog.pg_class c2, pg_catalog.pg_index i
LEFT JOIN pg_catalog.pg_constraint con
  ON (conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p','u','x'))
WHERE c.oid = '%s'::regclass AND c.oid = i.indrelid AND i.indexrelid = c2.oid
ORDER BY i.indisprimary DESC, c2.relname;`

const sqlDescribeReferences = `SELECT conname AS name, conrelid::regclass AS table,
  pg_catalog.pg_get_constraintdef(oid, true) AS definition
FROM pg_catalog.pg_constraint
WHERE confrelid = '%s'::regclass AND contype = 'f' AND conparentid = 0
ORDER BY conname;`

var statQueries = []struct {
	label string
	sql   string
}{
	{"Database sizes", `SELECT datname AS "DB Name", pg_size_pretty(pg_database_size(datname)) AS "DB Size" FROM pg_database ORDER BY pg_database_size(datname) DESC;`},
	{"Table sizes", `WITH RECURSIVE pg_inherit(inhrelid, inhparent) AS (SELECT inhrelid, inhparent FROM pg_inherits UNION SELECT child.inhrelid, parent.inhparent FROM pg_inherit child, pg_inherits parent WHERE child.inhparent = parent.inhrelid), pg_inherit_short AS (SELECT * FROM pg_inherit WHERE inhparent NOT IN (SELECT inhrelid FROM pg_inherit)) SELECT table_schema AS "Schema", table_name AS "Table", row_estimate AS "Row Estimate", pg_size_pretty(total_bytes) AS "Total", pg_size_pretty(index_bytes) AS "Index", pg_size_pretty(toast_bytes) AS "Toast", pg_size_pretty(table_bytes) AS "Table" FROM (SELECT *, total_bytes-index_bytes-coalesce(toast_bytes,0) AS table_bytes FROM (SELECT c.oid, nspname AS table_schema, relname AS table_name, sum(c.reltuples) OVER (PARTITION BY parent) AS row_estimate, sum(pg_total_relation_size(c.oid)) OVER (PARTITION BY parent) AS total_bytes, sum(pg_indexes_size(c.oid)) OVER (PARTITION BY parent) AS index_bytes, sum(pg_total_relation_size(reltoastrelid)) OVER (PARTITION BY parent) AS toast_bytes, parent FROM (SELECT pg_class.oid, reltuples, relname, relnamespace, pg_class.reltoastrelid, coalesce(inhparent, pg_class.oid) parent FROM pg_class LEFT JOIN pg_inherit_short ON inhrelid = oid WHERE relkind IN ('r', 'p')) c LEFT JOIN pg_namespace n ON n.oid = c.relnamespace) a WHERE oid = parent AND table_schema <> 'pg_catalog' AND table_schema <> 'information_schema') a ORDER BY total_bytes DESC;`},
	{"Running queries", `SELECT pid AS "PID", age(clock_timestamp(), query_start) AS "Age", usename AS "Username", query AS "Query" FROM pg_stat_activity WHERE query <> '<IDLE>' AND query NOT ILIKE '%pg_stat_activity%' ORDER BY query_start DESC;`},
	{"Index usage rates", `SELECT relname AS "Table name", 100 * idx_scan / (seq_scan + idx_scan) AS "Index Usage (%)", n_live_tup AS "Rows in Table" FROM pg_stat_user_tables WHERE seq_scan + idx_scan > 0 ORDER BY n_live_tup DESC;`},
	{"Unused indexes", `SELECT relname AS "Table Name", indexrelname AS "Index Name", idx_scan AS "Index Scans", idx_tup_read AS "Index Entries Returned", idx_tup_fetch AS "Live Rows Fetched", pg_size_pretty(pg_relation_size(indexrelname::regclass)) AS "Index Size" FROM pg_stat_all_indexes WHERE schemaname = 'public' AND idx_scan = 0 AND idx_tup_read = 0 AND idx_tup_fetch = 0 ORDER BY pg_relation_size(indexrelname::regclass) DESC;`},
}

func runBuiltinQuery(gabiUrl, bearerToken, sql string, out io.Writer, fancy bool, displayMode string) {
	result, err := queryGabi(gabiUrl, sql, bearerToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		return
	}
	if result.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.Error)
		return
	}
	formatResult(result, out, fancy, displayMode)
}

func describeTable(gabiUrl, bearerToken, tableName string, out io.Writer, fancy bool, displayMode string) {
	fmt.Fprintf(out, "Columns:\n")
	runBuiltinQuery(gabiUrl, bearerToken, fmt.Sprintf(sqlDescribeColumns, tableName), out, fancy, displayMode)
	fmt.Fprintln(out)

	fmt.Fprintf(out, "Indexes:\n")
	runBuiltinQuery(gabiUrl, bearerToken, fmt.Sprintf(sqlDescribeIndexes, tableName), out, fancy, displayMode)
	fmt.Fprintln(out)

	fmt.Fprintf(out, "Referenced by:\n")
	runBuiltinQuery(gabiUrl, bearerToken, fmt.Sprintf(sqlDescribeReferences, tableName), out, fancy, displayMode)
}

func runStats(gabiUrl, bearerToken, cmd string, out io.Writer, fancy bool, displayMode string) {
	fields := strings.Fields(cmd)
	if len(fields) > 1 {
		n := 0
		fmt.Sscanf(fields[1], "%d", &n)
		if n < 1 || n > len(statQueries) {
			fmt.Fprintf(os.Stderr, "Error: \\stats accepts 1-%d\n", len(statQueries))
			return
		}
		sq := statQueries[n-1]
		fmt.Fprintf(out, "%s:\n", sq.label)
		runBuiltinQuery(gabiUrl, bearerToken, sq.sql, out, fancy, displayMode)
		return
	}
	for _, sq := range statQueries {
		fmt.Fprintf(out, "%s:\n", sq.label)
		runBuiltinQuery(gabiUrl, bearerToken, sq.sql, out, fancy, displayMode)
		fmt.Fprintln(out)
	}
}

func runQuery(gabiUrl, bearerToken, input string, query *string, qh *QueryHistory, fancy bool, displayMode string, out io.Writer) {
	*query = fmt.Sprintf("%s%s", *query, input)
	if !strings.HasSuffix(*query, ";") {
		*query = fmt.Sprintf("%s\n", *query)
		return
	}
	*query = strings.TrimSpace(*query)
	if qh != nil {
		qh.AddQuery(*query)
	}
	result, err := queryGabi(gabiUrl, *query, bearerToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
	} else if result.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result.Error)
	} else {
		formatResult(result, out, fancy, displayMode)
	}
	*query = ""
}

func setupK8s(kubeconfigPath string) (clientcmd.ClientConfig, *restclient.Config) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfigPath
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})

	// use the current context in kubeconfig
	clientconfig, err := kubeconfig.ClientConfig()
	if err != nil {
		log.Fatal(err.Error())
	}
	return kubeconfig, clientconfig
}

func setDefaultNamespace(kubeconfig clientcmd.ClientConfig, namespace *string) {
	if *namespace == "" {
		var err error
		*namespace, _, err = kubeconfig.Namespace()
		if err != nil {
			log.Fatal(err.Error())
		}
	}
}

func getGabiRoute(config *restclient.Config, namespace string) (gabi routev1.Route, err error) {
	clientset, err := routeclientv1.NewForConfig(config)
	if err != nil {
		return
	}
	routes, err := clientset.Routes(namespace).List(context.TODO(), metav1.ListOptions{})

	if err != nil {
		return
	}

	for _, route := range routes.Items {
		if strings.HasPrefix(route.Name, "gabi-") {
			gabi = route
			return
		}
	}
	err = fmt.Errorf("no gabi route found in namespace %s", namespace)
	return
}

func gabiUrlFromRoute(route routev1.Route) string {
	var proto = "https"
	if route.Spec.TLS == nil {
		proto = "https"
	}
	return fmt.Sprintf("%s://%s%s", proto, route.Spec.Host, route.Spec.Path)
}

func queryGabi(url, query, token string) (models.QueryResponse, error) {
	reqModel := models.QueryRequest{Query: query}
	reqData, err := json.Marshal(reqModel)
	if err != nil {
		return models.QueryResponse{}, fmt.Errorf("marshal of query failed: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/query", url), bytes.NewReader(reqData))
	if err != nil {
		return models.QueryResponse{}, fmt.Errorf("request build failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return models.QueryResponse{}, fmt.Errorf("gabi request failed: %w", err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return models.QueryResponse{}, fmt.Errorf("http status: %s", resp.Status)
	}

	dec := json.NewDecoder(resp.Body)
	result := models.QueryResponse{}
	if e := dec.Decode(&result); e != nil {
		err = fmt.Errorf("malformed result %w", e)
	}
	return result, err
}

func getPager() string {
	if p := os.Getenv("PAGER"); p != "" {
		return p
	}
	return "less -R"
}

func getTerminalHeight() int {
	_, h, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

func pageOutput(content string) {
	lines := strings.Count(content, "\n")
	if lines < getTerminalHeight() {
		fmt.Print(content)
		return
	}
	pager := getPager()
	cmd := exec.Command("sh", "-c", pager)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Print(content)
	}
}

func getTerminalWidth() int {
	w, _, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func formatResult(r models.QueryResponse, out io.Writer, fancy bool, displayMode string) {
	if len(r.Result) == 0 {
		return
	}

	if displayMode == "expanded" {
		formatExpanded(r, out, fancy)
		return
	}

	if displayMode == "auto" {
		plain := renderTable(r, false)
		termWidth := getTerminalWidth()
		maxLineWidth := 0
		for _, line := range strings.Split(plain, "\n") {
			if len(line) > maxLineWidth {
				maxLineWidth = len(line)
			}
		}
		if maxLineWidth > termWidth {
			formatExpanded(r, out, fancy)
			return
		}
	}

	rendered := renderTable(r, fancy)
	fmt.Fprintln(out, rendered)
}

func renderTable(r models.QueryResponse, fancy bool) string {
	t := table.NewWriter()
	if len(r.Result) > 0 {
		t.AppendHeader(convertToRow(r.Result[0]))
	}
	if len(r.Result) > 1 {
		for _, row := range r.Result[1:] {
			t.AppendRow(convertToRow(row))
		}
	}
	if fancy {
		t.SetStyle(table.StyleRounded)
		t.Style().Color.Header = text.Colors{text.Bold, text.FgCyan}
	} else {
		t.Style().Options.DrawBorder = false
	}
	return t.Render()
}

func formatExpanded(r models.QueryResponse, out io.Writer, fancy bool) {
	if len(r.Result) < 2 {
		return
	}
	headers := r.Result[0]

	maxHeaderLen := 0
	for _, h := range headers {
		if len(h) > maxHeaderLen {
			maxHeaderLen = len(h)
		}
	}

	for i, row := range r.Result[1:] {
		label := fmt.Sprintf("-[ RECORD %d ]", i+1)
		padding := 40 - len(label)
		if padding < 3 {
			padding = 3
		}
		fmt.Fprintf(out, "%s%s\n", label, strings.Repeat("-", padding))

		for j, val := range row {
			header := ""
			if j < len(headers) {
				header = headers[j]
			}
			if fancy {
				fmt.Fprintf(out, "\033[1;36m%-*s\033[0m | %s\n", maxHeaderLen, header, val)
			} else {
				fmt.Fprintf(out, "%-*s | %s\n", maxHeaderLen, header, val)
			}
		}
		if i < len(r.Result)-2 {
			fmt.Fprintln(out)
		}
	}
}

func convertToRow(raw []string) (r table.Row) {
	r = make(table.Row, len(raw))
	for i, cell := range raw {
		r[i] = interface{}(cell)
	}
	return
}
