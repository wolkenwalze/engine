package main

import (
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "io/ioutil"
    "log"
    "math/rand"
    "net/http"
    "os"
    "path/filepath"
    "regexp"
    "sync"
    "time"

    corev1 "k8s.io/api/core/v1"
    v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/clientcmd"
    "k8s.io/client-go/util/homedir"
)

type Step struct {
    Id        string            `json:"id"`
    Type      string            `json:"type"`
    Params    map[string]string `json:"params"`
    NextSteps []string          `json:"nextSteps"`
    after     []string
}

type Result struct {
    Id                string `json:"result"`
    Success           bool   `json:"success"`
    Error             string `json:"error"`
    HTTPMonitorResult `json:",inline"`
    PodKillResult     `json:",inline"`
}

type HTTPMonitorResult struct {
    Latencies map[time.Time]int `json:"latencies,omitempty"`
}

type PodKillResult struct {
    PodNamespace string `json:"podNamespace,omitempty"`
    PodName      string `json:"podName,omitempty"`
}

type Workflow struct {
    InitialSteps []string `json:"initialSteps"`
    Steps        []Step   `json:"steps"`
    Results      []Result `json:"result"`
}

func main() {
    file := "wolkenwalze.json"
    outFile := "wolkenwwalze-results.json"
    flag.StringVar(&file, "file", file, "Config file to read")
    flag.StringVar(&outFile, "out", file, "Destination file to write")
    flag.Parse()

    data, err := ioutil.ReadFile(file)
    if err != nil {
        log.Fatal(err)
    }

    w := Workflow{}
    if err := json.Unmarshal(data, &w); err != nil {
        log.Fatal(err)
    }

    stepsByID := map[string]*Step{}
    for _, step := range w.Steps {
        stepsByID[step.Id] = &step
    }
    for id, step := range stepsByID {
        for _, nextStepID := range step.NextSteps {
            stepsByID[nextStepID].after = append(stepsByID[nextStepID].after, id)
        }
    }

    wg := &sync.WaitGroup{}
    lock := &sync.Mutex{}
    workPipe := make(chan Step)
    resultPipe := make(chan Result)

    for i := 0; i < 10; i++ {
        go stepExecutor(workPipe, resultPipe, lock, wg, stepsByID)
    }

    go func() {
        wg.Add(1)
        var results []Result
        for {
            result, ok := <-resultPipe
            if !ok {
                break
            }
            results = append(results, result)
        }
        w.Results = results

        outFH, err := os.Create(outFile)
        if err != nil {
            log.Fatalf("Failed to create outfile %s (%v)", outFile, err)
        }
        encoder := json.NewEncoder(outFH)
        if err := encoder.Encode(w); err != nil {
            log.Fatalf("Failed to write result file %s (%v)", outFile, err)
        }
    }()

    enqueueNextSteps(lock, stepsByID, workPipe, wg)

    wg.Wait()
    close(workPipe)
    close(resultPipe)
}

func stepExecutor(workPipe chan Step, resultPipe chan Result, lock *sync.Mutex, wg *sync.WaitGroup, stepsByID map[string]*Step) {
    for {
        step, ok := <-workPipe
        if !ok {
            return
        }

        switch step.Type {
        case "pod-kill":
            resultPipe <- executePodKill(step)
        case "http-monitor":
            resultPipe <- executeHTTPMonitor(step)
        }
        wg.Done()

        lock.Lock()
        for _, otherStep := range stepsByID {
            for i := range otherStep.after {
                if otherStep.after[i] == step.Id {
                    otherStep.after = append(otherStep.after[:i], otherStep.after[:i+1]...)
                }
            }
        }
        lock.Unlock()

        enqueueNextSteps(lock, stepsByID, workPipe, wg)
    }
}

func executeHTTPMonitor(step Step) (result Result) {
    t, ok := step.Params["time"]
    if !ok {
        t = "5m"
    }
    duration, err := time.ParseDuration(t)
    if err != nil {
        duration = 5 * time.Minute
    }
    target, ok := step.Params["target"]
    if !ok {
        return Result{
            Id:      step.Id,
            Success: false,
        }
    }
    timeout, cancel := context.WithTimeout(context.Background(), duration)
    defer cancel()
    latencies := map[time.Time]int{}
    failed := false
loop:
    for {
        startTime := time.Now()
        if _, err := http.Get(target); err != nil {
            latencies[time.Now()] = 0
            failed = true
        } else {
            endTime := time.Now()
            latencies[startTime] = int(endTime.Sub(startTime))
        }

        select {
        case <-time.After(time.Second):
        case <-timeout.Done():
            break loop
        }
    }
    return Result{
        Id:      step.Id,
        Success: !failed,
        HTTPMonitorResult: HTTPMonitorResult{
            Latencies: latencies,
        },
    }
}

func executePodKill(step Step) (result Result) {
    result = Result{
        Id: step.Id,
    }
    home := homedir.HomeDir()
    if home == "" {
        result.Error = fmt.Sprintf("Failed to find home directory")
        return result
    }
    kubeconfig := filepath.Join(home, ".kube", "config")

    config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
    if err != nil {
        result.Error = fmt.Sprintf("Failed to create kubeconfig from %s (%v)", kubeconfig, err)
        return result
    }
    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        result.Error = fmt.Sprintf("Failed to create k8s client (%v)", err)
        return result
    }

    namespaceRegex := regexp.MustCompile(".*")
    namespacePattern, ok := step.Params["namespace-pattern"]
    if ok {
        namespaceRegex, err = regexp.Compile(namespacePattern)
        if err != nil {
            result.Error = fmt.Sprintf("Invalid namespace-pattern: %s (%v)", namespaceRegex, err)
            return result
        }
    }

    nameRegex := regexp.MustCompile(".*")
    namePattern, ok := step.Params["name-pattern"]
    if ok {
        nameRegex, err = regexp.Compile(namePattern)
        if err != nil {
            result.Error = fmt.Sprintf("Invalid name-pattern: %s (%v)", namePattern, err)
            return result
        }
    }

    ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
    defer cancel()
    nsList, err := clientset.CoreV1().Namespaces().List(ctx, v1.ListOptions{})
    if err != nil {
        result.Error = fmt.Sprintf("Failed to list namespaces (%v)", err)
        return result
    }

    var possiblePods []corev1.Pod
    for _, ns := range nsList.Items {
        if namespaceRegex.Match([]byte(ns.Name)) {
            pods, err := clientset.CoreV1().Pods(ns.Name).List(ctx, v1.ListOptions{})
            if err != nil {
                result.Error = fmt.Sprintf("Failed to list pods in namespace %s (%v)", ns.Name, err)
                return result
            }
            for _, pod := range pods.Items {
                if nameRegex.Match([]byte(pod.Name)) {
                    possiblePods = append(possiblePods, pod)
                }
            }
        }
    }

    if len(possiblePods) == 0 {
        result.Error = fmt.Sprintf("No pods found matching specifications")
        return result
    }

    rand.Shuffle(len(possiblePods), func(i, j int) {
        possiblePods[i], possiblePods[j] = possiblePods[j], possiblePods[i]
    })

    target := possiblePods[0]

    if err := clientset.CoreV1().Pods(target.Namespace).Delete(ctx, target.Name, v1.DeleteOptions{}); err != nil {
        result.Error = fmt.Sprintf("Failed to remove pod %s in namespace %s (%v)", target.Name, target.Namespace, err)
        return result
    }
    result.PodName = target.Name
    result.PodNamespace = target.Namespace
    return result
}

func enqueueNextSteps(lock *sync.Mutex, steps map[string]*Step, pipe chan Step, wg *sync.WaitGroup) {
    lock.Lock()
    var result []*Step
    for _, step := range steps {
        if len(step.after) == 0 {
            delete(steps, step.Id)
            result = append(result, step)
        }
    }
    lock.Unlock()
    for _, step := range result {
        wg.Add(1)
        pipe <- *step
    }
}
