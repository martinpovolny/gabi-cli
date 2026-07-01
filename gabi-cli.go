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
	"github.com/google/shlex"
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

	dataDir := ""
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		dataDir = filepath.Join(d, "gabi-cli")
	} else if home := homedir.HomeDir(); home != "" {
		dataDir = filepath.Join(home, ".local", "share", "gabi-cli")
	}
	var qh *QueryHistory
	if dataDir != "" {
		os.MkdirAll(dataDir, 0700)
		qh = NewQueryHistory(filepath.Join(dataDir, "history"))
	}

	displayMode := *display

	var query string
	if len(flag.Args()) > 0 {
		query = strings.Join(flag.Args(), " ")
		runQuery(gabiUrl, bearerToken, "", &query, qh, *fancy, displayMode)
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
			runQuery(gabiUrl, bearerToken, edited, &query, qh, *fancy, displayMode)
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
		runQuery(gabiUrl, bearerToken, input, &query, qh, *fancy, displayMode)
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
	args, err := shlex.Split(editor)
	if err != nil || len(args) == 0 {
		return "", fmt.Errorf("failed to parse editor command: %s", editor)
	}
	args = append(args, tmpPath)
	cmd := exec.Command(args[0], args[1:]...)
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

func runQuery(gabiUrl, bearerToken, input string, query *string, qh *QueryHistory, fancy bool, displayMode string) {
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
		formatResult(result, os.Stdout, fancy, displayMode)
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
