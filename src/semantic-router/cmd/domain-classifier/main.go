// domain-classifier is the optional standalone deployment shape for the
// router's domain (category) signal: it wraps the in-process candle/mmBERT
// classifier behind the same http_classify wire contract the router already
// speaks (POST /classify with {"inputs": ...}, full label/score distribution
// out). Deploying it separately lets the routing decision plane stay a
// cgo-free pure-Go service (category_model.protocol: http_classify) while
// the model runs on a dedicated CPU or GPU host.
//
// The response must carry every label of the category mapping the router
// aligns against (scores validated to sum to ~1.0), so this service loads
// the same mapping file the router uses.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	candle_binding "github.com/vllm-project/semantic-router/candle-binding"
	"github.com/vllm-project/semantic-router/src/semantic-router/pkg/classification"
)

type classifyRequest struct {
	Inputs string `json:"inputs"`
}

type labelScore struct {
	Label string  `json:"label"`
	Score float32 `json:"score"`
}

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	modelPath := flag.String("model", "", "category model directory (ModernBERT-format weights)")
	mappingPath := flag.String("mapping", "", "category mapping JSON (category_to_idx / idx_to_category)")
	variant := flag.String("variant", "modernbert", "model family: modernbert | candle_bert")
	numClasses := flag.Int("num-classes", 0, "class count for -variant=candle_bert (defaults to the mapping size)")
	useCPU := flag.Bool("use-cpu", true, "run inference on CPU")
	accessKey := flag.String("access-key", os.Getenv("DOMAIN_CLASSIFIER_ACCESS_KEY"), "optional bearer access key")
	flag.Parse()

	if strings.TrimSpace(*modelPath) == "" || strings.TrimSpace(*mappingPath) == "" {
		log.Fatalf("both -model and -mapping are required")
	}

	mapping, err := classification.LoadCategoryMapping(*mappingPath)
	if err != nil {
		log.Fatalf("failed to load category mapping: %v", err)
	}
	if n := mapping.LabelCount(); n < 2 {
		log.Fatalf("category mapping must define at least 2 labels, got %d", n)
	}

	var classify func(text string) (candle_binding.ClassResultWithProbs, error)
	switch *variant {
	case "modernbert":
		if err := candle_binding.InitModernBertClassifier(*modelPath, *useCPU); err != nil {
			log.Fatalf("failed to initialize ModernBERT classifier: %v", err)
		}
		classify = candle_binding.ClassifyModernBertTextWithProbabilities
	case "candle_bert":
		classes := *numClasses
		if classes == 0 {
			classes = mapping.LabelCount()
		}
		if !candle_binding.InitCandleBertClassifier(*modelPath, classes, *useCPU) {
			log.Fatalf("failed to initialize Candle BERT classifier")
		}
		classify = candle_binding.ClassifyTextWithProbabilities
	default:
		log.Fatalf("unsupported -variant %q (supported: modernbert, candle_bert)", *variant)
	}
	log.Printf("domain classifier ready: variant=%s model=%s labels=%d addr=%s", *variant, *modelPath, mapping.LabelCount(), *addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/classify", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, *accessKey) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req classifyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Inputs) == "" {
			http.Error(w, `invalid request: expected {"inputs": "..."}`, http.StatusBadRequest)
			return
		}

		result, err := classify(req.Inputs)
		if err != nil {
			http.Error(w, fmt.Sprintf("classification failed: %v", err), http.StatusInternalServerError)
			return
		}

		scores := make([]labelScore, 0, len(result.Probabilities))
		for idx, score := range result.Probabilities {
			label, ok := mapping.LabelFromIndex(idx)
			if !ok {
				http.Error(w, fmt.Sprintf("model class index %d missing from category mapping", idx), http.StatusInternalServerError)
				return
			}
			scores = append(scores, labelScore{Label: label, Score: score})
		}
		sort.SliceStable(scores, func(i, j int) bool { return scores[i].Score > scores[j].Score })

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scores)
	})

	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func authorized(r *http.Request, accessKey string) bool {
	if accessKey == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+accessKey
}
