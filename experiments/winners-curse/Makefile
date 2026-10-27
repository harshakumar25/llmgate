.PHONY: data sweep llm random seeds compare all

data:     ## Download BEIR SciFact (2.8 MB) into data/scifact
	mkdir -p data && curl -sSfL -o data/scifact.zip \
	  https://public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/scifact.zip && \
	  cd data && unzip -q -o scifact.zip && rm scifact.zip

sweep:    ## Random-search pool replayed over many splits (no LLM needed)
	python3 experiment.py sweep

llm:      ## LLM-proposed search through LLMGate on :8090 (run the gateway first)
	python3 experiment.py search --proposer llm --candidates 60 --out results/llm

random:   ## Same split and budget as `llm`, random proposer
	python3 experiment.py search --proposer random --candidates 60 --out results/random

seeds:    ## Paired random + LLM searches on three more splits
	for s in 11 13 17; do \
	  python3 experiment.py --seed $$s search --proposer random --candidates 60 --out results/seeds/random-$$s && \
	  python3 experiment.py --seed $$s search --proposer llm --candidates 60 --out results/seeds/llm-$$s; \
	done

compare:  ## Claimed vs real gain across every search run
	python3 experiment.py compare results/llm results/random results/seeds/*

all: sweep random llm seeds compare
