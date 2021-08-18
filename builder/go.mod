module github.com/redhat-openshift-ecosystem/community-operators-prod/builder

go 1.16

require (
	github.com/Shopify/logrus-bugsnag v0.0.0-20171204204709-577dee27f20d // indirect
	github.com/blang/semver/v4 v4.0.0
	github.com/bshuster-repo/logrus-logstash-hook v1.0.2 // indirect
	github.com/operator-framework/operator-registry v0.0.0
	github.com/sirupsen/logrus v1.8.1
	golang.org/x/sync v0.0.0-20201020160332-67f06af15bc9
	k8s.io/apimachinery v0.20.6
	k8s.io/client-go v0.20.6
	rsc.io/letsencrypt v0.0.3 // indirect
	sigs.k8s.io/yaml v1.2.0
)

replace github.com/operator-framework/operator-registry => github.com/joelanford/operator-registry v1.12.2-0.20210721025046-a8d185d7a62c
