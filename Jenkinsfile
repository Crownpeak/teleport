pipeline {
    agent { label 'qa_docker' }
    options {
        checkoutToSubdirectory('src')
        disableConcurrentBuilds()
        buildDiscarder(logRotator(numToKeepStr: '10'))
    }
    parameters {
        string(name: 'CHECKOUT_BRANCH',
               defaultValue: 'add-oidc-support-v18.5.1-v2')
        string(name: 'TAG_PUSH_VERSION',
               defaultValue: '18.5.1-oidc')
    }
    environment {
        GIT_CREDENTIALS_ID = 'ec2-user'
        DOCKER_REGISTRY = 'intranet.fredhopper.com/teleport'
        // Architecture for the build
        ARCH = 'amd64'
        // Teleport buildbox version (must match the version in build.assets/images.mk)
        BUILDBOX_VERSION = 'teleport18'
        BUILDBOX_BASE = 'ghcr.io/gravitational/teleport-buildbox'
        // Go build parallelism - set to number of CPU cores
        GOMAXPROCS = '8'
    }
    stages {
        stage('Prepare Workspace') {
            steps {
                script {
                    wrap([$class: 'BuildUser']) {
                        currentBuild.description = "Username: ${env.BUILD_USER}"
                    }
                }
            }
        }
        stage('Clone Repository') {
            steps {
                sshagent(credentials: [GIT_CREDENTIALS_ID]) {
                    sh '''
                        # Shallow clone to speed up checkout
                        git clone --depth 1 --branch "${CHECKOUT_BRANCH}" https://github.com/Crownpeak/teleport
                        cd teleport
                        git log -1 --oneline
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
        stage('Build Web Assets and Binaries') {
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
                        
                        # Clean Rust target directory to avoid GLIBC version conflicts
                        echo "=== Cleaning Rust build artifacts ==="
                        rm -rf target/
                        
                        echo "=== Building Teleport binaries inside Docker ==="
                        
                        # Build only required binaries (teleport, tctl, tsh, tbot, fdpass-teleport)
                        # Skip teleport-update as it's removed from Docker image anyway
                        # Use parallel Go compilation and caching
                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp:/tmp \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            -e GOCACHE=/tmp/go-cache \
                            -e GOMAXPROCS=${GOMAXPROCS} \
                            ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} \
                            make -j${GOMAXPROCS} WEBASSETS_SKIP_BUILD=1 build/teleport build/tctl build/tsh build/tbot build/fdpass-teleport
                        
                        echo "=== Build complete, checking output ==="
                        ls -la build/
                    '''
                }
            }
        }
        stage('Create DEB Package') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Creating DEB package on host ==="
                        
                        # Get the version from the Makefile
                        VERSION=$(grep "^VERSION=" Makefile | cut -d= -f2)
                        echo "Teleport version: ${VERSION}"
                        
                        # Run make deb directly on the host where Docker is available
                        # The build-package.sh script will use Docker to run fpm for packaging
                        make deb
                        
                        echo "=== DEB package created ==="
                        ls -la build/*.deb
                    '''
                }
            }
        }
        stage('Build Docker Image') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Building Docker image ==="
                        
                        # Get the version from the Makefile
                        VERSION=$(grep "^VERSION=" Makefile | cut -d= -f2)
                        echo "Teleport version: ${VERSION}"
                        
                        # Copy the Dockerfile to build directory
                        cp ./build.assets/charts/Dockerfile build/
                        
                        # Build the Docker image (enable BuildKit for --mount support)
                        cd build
                        DOCKER_BUILDKIT=1 docker build --no-cache . \
                            -t ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION} \
                            --target teleport \
                            --build-arg DEB_PATH="./teleport_${VERSION}_${ARCH}.deb"
                        
                        echo "=== Docker image built successfully ==="
                        docker images | grep ${DOCKER_REGISTRY}
                    '''
                }
            }
        }
        stage('Push Docker Image') {
            steps {
                sh '''
                    echo "=== Pushing Docker image ==="
                    docker login -u docker -p docker ${DOCKER_REGISTRY}
                    docker push ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION}
                    echo "=== Image pushed successfully ==="
                '''
            }
        }
    }
    post {
        always {
            sh '''
                # Clean up Docker images to save space
                docker rmi ${DOCKER_REGISTRY}:${TAG_PUSH_VERSION} || true
                docker rmi ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} || true
                docker rmi ${BUILDBOX_BASE}-node:${BUILDBOX_VERSION} || true
                docker system prune -f || true
            '''
        }
        cleanup {
            deleteDir()
        }
    }
}
