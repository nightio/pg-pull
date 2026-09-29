.PHONY: test test-integration vet build dist

test:
	mage test

test-integration:
	mage integration

vet:
	mage vet

build:
	mage build

dist:
	mage dist
