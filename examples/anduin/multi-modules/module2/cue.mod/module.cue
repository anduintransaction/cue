module: "dev.anduintransact.com/module2@v1"
language: {
	version: "v0.11.0"
}
source: {
	kind: "self"
}
deps: {
	"dev.anduintransact.com/module1@v1": {
		v:       "v1.0.1"
		default: true
	}
}
