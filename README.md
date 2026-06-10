# K-Sentinel

## Visao Geral

K-Sentinel e um Kubernetes Operator escrito em Go que automatiza o provisionamento de monitores no Datadog para microsservicos implantados no cluster. O objetivo e tratar Observabilidade como Codigo: eliminar o trabalho manual de configurar alertas a cada novo deployment e garantir que o ciclo de vida do monitoramento esteja vinculado ao ciclo de vida da aplicacao.

O Operator observa recursos do tipo `apps/v1 Deployment` em todo o cluster. Quando um Deployment possui a anotacao `kubeobserver.io/monitor: "true"`, o K-Sentinel provisiona automaticamente o conjunto padrao de monitores na API do Datadog. Quando o Deployment e removido, os monitores correspondentes sao deletados antes que o Kubernetes finalize a exclusao do objeto.

Nao ha CRDs customizados. O contrato entre as equipes de engenharia e o Operator e inteiramente declarado por anotacoes no proprio `Deployment`.

### Anotacoes suportadas

| Anotacao | Obrigatoria | Descricao |
|---|---|---|
| `kubeobserver.io/monitor: "true"` | Sim | Ativa o Operator para este Deployment |
| `kubeobserver.io/team: "<nome>"` | Nao | Define o time responsavel pelos alertas gerados |

---

## Decisoes de Arquitetura

### Framework

O projeto e construido com **Go 1.25+**, **Kubebuilder v4** e **controller-runtime v0.23**. O controller-runtime fornece o loop de reconciliacao, o cache de objetos Kubernetes via informers, o mecanismo de retry com backoff exponencial e o servidor de health probes, todos configurados sem boilerplate adicional.

O ponto de entrada (`cmd/main.go`) instancia o Manager, registra o `DeploymentReconciler` com o cliente Datadog injetado via interface e bloqueia ate receber sinal de termino.

### Resiliencia via Kubernetes Finalizers

O Operator registra o finalizer `kubeobserver.io/finalizer` nos Deployments gerenciados antes de qualquer chamada a API do Datadog. Esse mecanismo garante a seguinte invariante:

> Um Deployment nao pode ser removido do cluster enquanto existirem monitores ativos associados a ele no Datadog.

Quando `kubectl delete deployment` e executado, o API server marca o objeto com `DeletionTimestamp` e aguarda. O Operator detecta o campo preenchido, chama `DeleteMonitors` e somente remove o finalizer apos receber confirmacao de sucesso da API. Se `DeleteMonitors` retornar erro, o finalizer permanece e o controller-runtime recoloca o objeto na fila com backoff exponencial. O Deployment permanece em estado `Terminating` ate que a limpeza seja confirmada, prevenindo monitores orfaos.

O finalizer tambem e registrado antes da primeira chamada a `CreateMonitors`. Se o processo for interrompido entre o registro do finalizer e a criacao dos monitores, a proxima reconciliacao encontrara o finalizer presente e retomara o fluxo corretamente sem perda de estado.

### Idempotencia

A integracao com o Datadog implementa idempotencia em dois niveis:

**Criacao (`CreateMonitors`):** Antes de cada chamada `CreateMonitor`, o cliente busca na conta do Datadog um monitor com o nome exato que seria criado. A API do Datadog retorna correspondencias por substring; o codigo filtra por igualdade exata do campo `name`. Se o monitor ja existe, a criacao e ignorada. Isso torna cada monitor individualmente idempotente: falhas parciais sao recuperaveis sem duplicatas, pois monitores ja existentes sao pulados e apenas os ausentes sao criados.

**Delecao (`DeleteMonitors`):** A busca utiliza o prefixo `[k-sentinel] <appName>` como ancora. Apos receber a lista da API, o codigo aplica um filtro `strings.HasPrefix` adicional para descartar correspondencias por substring que pudessem atingir monitores de outras aplicacoes, por exemplo, `api` correspondendo a `api-gateway`.

### Seguranca

**Imagem de container:** O Dockerfile utiliza build multi-stage. O stage de compilacao usa `golang:1.25` com `CGO_ENABLED=0`, `-trimpath` e `-ldflags="-w -s"` para produzir um binario estatico sem informacoes de debug ou caminhos locais embutidos. O stage de runtime usa `gcr.io/distroless/static:nonroot`, uma imagem sem shell, sem gerenciador de pacotes e sem usuario root. O processo executa com UID 65532.

**RBAC:** As permissoes do Operator seguem o Principio do Menor Privilegio. O `ClusterRole` gerado por `make manifests` a partir dos markers do controller contem apenas:

```yaml
rules:
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get, list, watch, update, patch]

  - apiGroups: [apps]
    resources: [deployments/status]
    verbs: [get]

  - apiGroups: [apps]
    resources: [deployments/finalizers]
    verbs: [update]
```

Os verbos `create` e `delete` sobre `deployments` foram removidos deliberadamente. O Operator nunca cria nem deleta Deployments. A permissao de escrita em `deployments/status` tambem foi removida pois o Operator nao utiliza esse subrecurso.

**Credenciais:** `DD_API_KEY` e `DD_APP_KEY` sao injetadas no container via `secretKeyRef` apontando para um Secret Kubernetes chamado `datadog-secrets`. As chaves nunca aparecem como variaveis de ambiente literais no manifesto do Deployment nem em imagens Docker.

### Extensibilidade

A camada de integracao com o Datadog e acessada pelo controller exclusivamente atraves da interface `observability.MonitorClient`:

```go
type MonitorClient interface {
    CreateMonitors(ctx context.Context, appName string, team string) error
    DeleteMonitors(ctx context.Context, appName string) error
}
```

Para substituir o backend de observabilidade, basta implementar essa interface e alterar a instanciacao em `cmd/main.go`. O controller permanece inalterado.

---

## Instrucoes de Execucao

### Pre-requisitos

- Go 1.22 ou superior
- Docker
- kubectl
- Minikube ou outro cluster local compativel
- make

### 1. Iniciar o cluster local

```bash
minikube start
```

Para utilizar a imagem construida localmente sem um registry externo, aponte o cliente Docker para o daemon interno do Minikube antes de prosseguir:

```bash
eval $(minikube docker-env)
```

### 2. Construir a imagem do Operator

```bash
make docker-build IMG=k-sentinel:latest
```

### 3. Provisionar o Secret com as credenciais do Datadog

Crie o namespace antes de aplicar o Secret, pois o `make deploy` pode nao garantir a ordem de criacao em todos os ambientes:

```bash
kubectl create namespace k-sentinel-system
```

```bash
kubectl create secret generic datadog-secrets \
  --from-literal=api-key=<SEU_DD_API_KEY> \
  --from-literal=app-key=<SEU_DD_APP_KEY> \
  --namespace k-sentinel-system
```

### 4. Implantar o Operator no cluster

```bash
make deploy IMG=k-sentinel:latest
```

O comando executa `make manifests` internamente para garantir que os manifestos RBAC estejam sincronizados com os markers do controller e em seguida aplica o overlay Kustomize de `config/default` no cluster, criando o namespace `k-sentinel-system`, o `ServiceAccount`, o `ClusterRole`, o `ClusterRoleBinding` e o `Deployment` do Operator.

### 5. Verificar a inicializacao

```bash
kubectl get pods -n k-sentinel-system
```

O pod `k-sentinel-controller-manager-*` deve atingir o status `Running`. Para confirmar que a conexao com o Datadog foi estabelecida com sucesso, inspecione os logs:

```bash
kubectl logs -n k-sentinel-system \
  -l control-plane=controller-manager \
  --follow
```

---

## Exemplo de Uso

Para ativar o monitoramento automatico em um Deployment, adicione as anotacoes abaixo ao campo `metadata.annotations`. Nenhuma outra alteracao na especificacao da aplicacao e necessaria.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: payment-service
  namespace: default
  annotations:
    kubeobserver.io/monitor: "true"
    kubeobserver.io/team: "payments"
spec:
  replicas: 2
  selector:
    matchLabels:
      app: payment-service
  template:
    metadata:
      labels:
        app: payment-service
    spec:
      containers:
        - name: payment-service
          image: payment-service:1.0.0
          ports:
            - containerPort: 8080
```

Apos o `kubectl apply`, o Operator detecta o Deployment na proxima reconciliacao, registra o finalizer `kubeobserver.io/finalizer` no objeto e provisiona os monitores no Datadog. O monitor criado sera nomeado `[k-sentinel] payment-service - Pod Restart Rate High` e incluira as tags `managed-by:k-sentinel`, `kube_deployment:payment-service` e `team:payments`.

Para encerrar o monitoramento sem remover o Deployment, remova a anotacao `kubeobserver.io/monitor`. O Operator ignorara o objeto nas reconciliacoes seguintes, mas os monitores existentes nao serao deletados automaticamente nesse cenario. Para deletar os monitores via exclusao do Deployment:

```bash
kubectl delete deployment payment-service
```

O Deployment permanecera em estado `Terminating` ate que o Operator confirme a exclusao dos monitores no Datadog e libere o finalizer.

---

## Estrutura do Repositorio

```
.
├── cmd/main.go                          # Entrypoint: inicializa Manager e injeta DatadogClient
├── internal/controller/
│   └── deployment_controller.go         # Reconciler: filtro de anotacao, finalizer, upsert/delete
├── pkg/observability/
│   ├── client.go                        # Interface MonitorClient
│   └── datadog.go                       # Implementacao DatadogClient com idempotencia por nome
├── config/
│   ├── default/kustomization.yaml       # Overlay principal (namespace: k-sentinel-system)
│   ├── rbac/role.yaml                   # ClusterRole gerado por make manifests
│   └── manager/
│       ├── manager.yaml                 # Deployment do Operator com env DD_* via secretKeyRef
│       └── datadog-secret.yaml          # Template do Secret (nao commitar valores reais)
├── Dockerfile                           # Multi-stage: golang:1.25 builder + distroless:nonroot
└── Makefile                             # Targets: manifests, build, docker-build, deploy
```

---

## Licenca

Copyright 2026. Licenciado sob os termos da Apache License, Version 2.0.

Criado por João Breno
