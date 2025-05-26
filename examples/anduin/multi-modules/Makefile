MODULE_1_VERSION:=v1.0.3
MODULE_2_VERSION:=v1.0.0

nuke:
	sudo rm -rf ./cue.cache/mod && \
		rm -rf ./data/* && \
		mkdir -p ./cue.cache ./data

publish-module1:
	cd ./module1 && \
		../cue.sh mod publish ${MODULE_1_VERSION}

publish-module2:
	cd ./module2 && \
		../cue.sh mod publish ${MODULE_2_VERSION}

eval-module2:
	cd ./module2 && \
		../cue.sh mod tidy && \
		../cue.sh export ./main.cue

oras-pull:
	cd data && \
		oras pull localhost:5000/anduin.com/module1:${MODULE_1_VERSION}
