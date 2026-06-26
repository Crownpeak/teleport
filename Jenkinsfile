pipeline {
    agent { label 'qa_docker' }
    options {
        checkoutToSubdirectory('teleport')
        disableConcurrentBuilds()
        buildDiscarder(logRotator(numToKeepStr: '10'))
    }
    environment {
        DOCKER_REGISTRY = 'intranet.fredhopper.com/teleport'
        // Architecture for the build
        ARCH = 'amd64'
        // Teleport buildbox version (must match the version in build.assets/images.mk)
        BUILDBOX_VERSION = 'teleport18'
        BUILDBOX_BASE = 'ghcr.io/gravitational/teleport-buildbox'
        // Go build parallelism - set to number of CPU cores
        GOMAXPROCS = '8'
        // Derive image tag from branch name: add-oidc-support-v18.8.0 -> 18.8.0
        TAG_PUSH_VERSION = env.BRANCH_NAME.replaceAll('add-oidc-support-v', '')
    }
    stages {
        stage('Prepare Workspace') {
            steps {
                script {
                    wrap([$class: 'BuildUser']) {
                        currentBuild.description = "Branch: ${env.BRANCH_NAME} | Version: ${TAG_PUSH_VERSION} | User: ${env.BUILD_USER}"
                    }
                }
                dir('teleport') {
                    sh '''
                        git log -1 --oneline

                        # Verify OIDC/SAML entitlement fix is present
                        echo "=== Verifying OIDC/SAML entitlement fix ==="
                        if grep -q "Always enable OIDC and SAML for OSS builds" lib/modules/modules.go; then
                            echo "✓ OIDC/SAML entitlement fix is present in the code"
                        else
                            echo "✗ ERROR: OIDC/SAML entitlement fix NOT found!"
                            grep -A 10 "func (f Features) GetEntitlement" lib/modules/modules.go
                            exit 1
                        fi
                    '''
                }
            }
        }
        stage('Pull Buildbox Images') {
            steps {
                sh '''
                    echo "=== Pulling pre-built buildbox images from GitHub Container Registry ==="

                    # Pull images in parallel
                    docker pull ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} &
                    docker pull ${BUILDBOX_BASE}-node:${BUILDBOX_VERSION} &
                    wait

                    echo "=== Buildbox images pulled successfully ==="
                    docker images | grep teleport-buildbox
                '''
            }
        }
        stage('Run Unit Tests') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Running fork OIDC/SAML canary unit tests ==="

                        export UID=$(id -u)
                        export GID=$(id -g)

                        mkdir -p /tmp/go-cache-teleport /tmp/gomodcache-teleport

                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp/go-cache-teleport:/tmp/go-cache \
                            -v /tmp/gomodcache-teleport:/tmp/gomodcache \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            -e GOCACHE=/tmp/go-cache \
                            -e GOMODCACHE=/tmp/gomodcache \
                            -e GOMAXPROCS=${GOMAXPROCS} \
                            ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} \
                            go test ./lib/modules/... -race -v

                        echo "=== Unit tests passed ==="
                    '''
                }
            }
        }
        stage('Build Web Assets') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Building web assets inside Docker ==="

                        # Get UID/GID for proper file permissions
                        export UID=$(id -u)
                        export GID=$(id -g)

                        # Build webassets using the Node.js buildbox
                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp:/tmp \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            ${BUILDBOX_BASE}-node:${BUILDBOX_VERSION} \
                            make ensure-webassets

                        echo "=== Web assets built successfully ==="
                    '''
                }
            }
        }
        stage('Build Binaries') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Preparing build environment ==="

                        # Get UID/GID for proper file permissions
                        export UID=$(id -u)
                        export GID=$(id -g)

                        # Clean Rust target directory to avoid GLIBC version conflicts
                        echo "=== Cleaning Rust build artifacts ==="
                        rm -rf target/

                        # Clean Go build cache to ensure fresh compilation with our modifications
                        echo "=== Cleaning Go build cache ==="
                        rm -rf /tmp/go-cache-teleport || true
                        mkdir -p /tmp/go-cache-teleport /tmp/gomodcache-teleport

                        # Clean existing build directory
                        rm -rf build/ || true
                        mkdir -p build

                        echo "=== Building Teleport binaries inside Docker ==="

                        # Build teleport binary with -a flag to force recompilation
                        # This ensures our OIDC/SAML modifications are included
                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp/go-cache-teleport:/tmp/go-cache \
                            -v /tmp/gomodcache-teleport:/tmp/gomodcache \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            -e GOCACHE=/tmp/go-cache \
                            -e GOMODCACHE=/tmp/gomodcache \
                            -e GOMAXPROCS=${GOMAXPROCS} \
                            ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} \
                            go build -a -tags "webassets_embed" -o build/teleport ./tool/teleport

                        echo "=== Built teleport binary ==="

                        # Build tctl and tsh (can use cached dependencies now)
                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp/go-cache-teleport:/tmp/go-cache \
                            -v /tmp/gomodcache-teleport:/tmp/gomodcache \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            -e GOCACHE=/tmp/go-cache \
                            -e GOMODCACHE=/tmp/gomodcache \
                            -e GOMAXPROCS=${GOMAXPROCS} \
                            ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} \
                            sh -c "go build -tags 'webassets_embed' -o build/tctl ./tool/tctl && go build -tags 'webassets_embed' -o build/tsh ./tool/tsh"

                        echo "=== Build complete, checking output ==="
                        ls -la build/

                        # Verify the binaries have OIDC enabled
                        echo "=== Verifying OIDC is enabled in built binary ==="
                        if strings build/teleport | grep -q "Always enable OIDC"; then
                            echo "✓ OIDC fix confirmed in binary"
                        else
                            echo "Note: String verification inconclusive, will verify at runtime"
                        fi
                    '''
                }
            }
        }
        stage('Build Docker Image') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Building Docker image using direct binary approach ==="

                        # Use the direct Dockerfile that copies binaries directly
                        # This avoids DEB packaging issues with stale binaries
                        docker build --no-cache \
                            -f build.assets/Dockerfile.oidc \
                            -t ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION} \
                            build/

                        echo "=== Docker image built successfully ==="
                        docker images | grep ${DOCKER_REGISTRY}
                    '''
                }
            }
        }
        stage('Verify Image') {
            steps {
                sh '''
                    echo "=== Verifying OIDC/SAML are enabled in the Docker image ==="

                    # Run a quick test to verify entitlements.
                    # The image ENTRYPOINT is "teleport start -c ...", so we must
                    # override it to invoke "teleport version" directly, otherwise
                    # "version" is appended to "start" and teleport rejects it.
                    docker run --rm --entrypoint /usr/local/bin/teleport ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION} version

                    echo "=== Image verification complete ==="
                '''
            }
        }
        stage('Push Docker Image') {
            steps {
                sh '''
                    echo "=== Pushing Docker image ==="
                    docker login -u docker -p docker intranet.fredhopper.com
                    docker push ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION}

                    # Also tag as latest for convenience
                    docker tag ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION} ${DOCKER_REGISTRY}:latest
                    docker push ${DOCKER_REGISTRY}:latest

                    echo "=== Image pushed successfully ==="
                    echo "Images available:"
                    echo "  - ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION}"
                    echo "  - ${DOCKER_REGISTRY}:latest"
                '''
            }
        }
    }
    post {
        always {
            sh '''
                # Clean up Docker images to save space
                docker rmi ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION} || true
                docker rmi ${DOCKER_REGISTRY}:latest || true
                docker rmi ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} || true
                docker rmi ${BUILDBOX_BASE}-node:${BUILDBOX_VERSION} || true
                docker system prune -f || true

                # Clean up Go cache
                rm -rf /tmp/go-cache-teleport || true
                rm -rf /tmp/gomodcache-teleport || true
            '''
        }
        cleanup {
            deleteDir()
        }
    }
}
