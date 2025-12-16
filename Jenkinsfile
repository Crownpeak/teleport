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
        // Architecture for the build (amd64, arm64)
        ARCH = 'amd64'
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
        stage('Build Binaries') {
            steps {
                dir('teleport') {
                    sh '''
                        echo "=== Building Teleport binaries using official build process ==="
                        
                        # Build binaries inside Docker (uses CentOS 7 buildbox for glibc compatibility)
                        # This also builds webassets (web UI)
                        make docker-binaries
                        
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
                        echo "=== Creating DEB package ==="
                        
                        # Build the .deb package needed for the Docker image
                        make oss-deb ARCH=${ARCH}
                        
                        echo "=== DEB package created ==="
                        ls -la build/*.deb
                    '''
                }
            }
        }
        stage('Build Docker Image') {
            steps {
                dir('teleport') {
                    withCredentials([usernamePassword(credentialsId: 'github-automation-test', 
                                                       usernameVariable: 'USERNAME', 
                                                       passwordVariable: 'PASSWORD')]) {
                        sh '''
                            echo "=== Building Docker image ==="
                            
                            VERSION=$(make print-version)
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
                docker system prune -f || true
            '''
        }
        cleanup {
            deleteDir()
        }
    }
}
