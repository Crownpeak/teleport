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
                        git clone https://github.com/Crownpeak/teleport
                        cd teleport
                        git checkout "${CHECKOUT_BRANCH}"
                        git log -1 --oneline
                    '''
                }
            }
        }
        stage('Pull Buildbox Images') {
            steps {
                sh '''
                    echo "=== Pulling pre-built buildbox images from GitHub Container Registry ==="
                    
                    # Pull the CentOS 7 buildbox (for compiling Go binaries)
                    docker pull ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH}
                    
                    # Pull the Node.js buildbox (for building web UI)
                    docker pull ${BUILDBOX_BASE}-node:${BUILDBOX_VERSION}
                    
                    echo "=== Buildbox images pulled successfully ==="
                    docker images | grep teleport-buildbox
                '''
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
                        # This follows the official build process from build.assets/Makefile
                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp:/tmp \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            ${BUILDBOX_BASE}-node:${BUILDBOX_VERSION} \
                            make ensure-webassets
                        
                        echo "=== Web assets built successfully ==="
                        ls -la webassets/ || echo "webassets directory not found, checking web/packages"
                    '''
                }
            }
        }
        stage('Build Binaries') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Building Teleport binaries inside Docker ==="
                        
                        # Clean Rust target directory to avoid GLIBC version conflicts
                        # The Node.js buildbox has newer GLIBC than CentOS 7 buildbox,
                        # so we must rebuild Rust artifacts from scratch
                        echo "=== Cleaning Rust build artifacts ==="
                        rm -rf target/
                        
                        # Get UID/GID for proper file permissions
                        export UID=$(id -u)
                        export GID=$(id -g)
                        
                        # Build binaries using the CentOS 7 buildbox
                        # This follows the official build process from build.assets/Makefile
                        docker run --rm \
                            -v "$(pwd)":/go/src/github.com/gravitational/teleport \
                            -v /tmp:/tmp \
                            -w /go/src/github.com/gravitational/teleport \
                            -u ${UID}:${GID} \
                            -e HOME=/tmp \
                            -e GOCACHE=/tmp/go-cache \
                            ${BUILDBOX_BASE}-centos7:${BUILDBOX_VERSION}-${ARCH} \
                            make full
                        
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
                        
                        # Build the Docker image
                        cd build
                        docker build --no-cache . \
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
