.PHONY: test lint

test:
	python -m pytest --cov=. --cov-report=term-missing

lint:
	ruff format --check .
	ruff check .
	mypy .
