package jsonutil

import (
	"encoding/json"
	"fmt"
)

type DiffResult struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Action string `json:"action"` // "add", "reget"
	Name   string `json:"name"`
}

// findDifferences 比较两个JSON树结构，以a为基准
func findDifferences(a, b map[string]interface{}) []DiffResult {
	var diffs []DiffResult

	// 递归比较函数
	var compare func(nodeA, nodeB map[string]interface{}, parentPath string)
	compare = func(nodeA, nodeB map[string]interface{}, parentPath string) {
		// 获取当前节点信息
		nameA, _ := nodeA["name"].(string)
		pathA, _ := nodeA["path"].(string)
		typeA, _ := nodeA["type"].(string)

		currentPath := pathA
		if parentPath != "" {
			currentPath = parentPath + "/" + nameA
		}

		// 如果b中没有对应节点，标记为add
		if nodeB == nil {
			if typeA == "file" {
				diffs = append(diffs, DiffResult{
					Path:   currentPath,
					Type:   typeA,
					Action: "add",
					Name:   nameA,
				})
			}

			// 递归处理所有子节点
			childrenA, okA := nodeA["children"].([]interface{})
			if okA {
				for _, childA := range childrenA {
					if childAMap, ok := childA.(map[string]interface{}); ok {
						compare(childAMap, nil, currentPath)
					}
				}
			}
			return
		}

		// 比较基本属性
		nameB, _ := nodeB["name"].(string)
		typeB, _ := nodeB["type"].(string)

		if nameA != nameB || typeA != typeB {
			if typeA == "file" {
				diffs = append(diffs, DiffResult{
					Path:   currentPath,
					Type:   typeA,
					Action: "reget",
					Name:   nameA,
				})
			}

		}

		// 比较children
		childrenA, okA := nodeA["children"].([]interface{})
		childrenB, okB := nodeB["children"].([]interface{})

		if !okA {
			childrenA = []interface{}{}
		}
		if !okB {
			childrenB = []interface{}{}
		}

		// 将childrenB转换为map以便快速查找
		childrenBMap := make(map[string]map[string]interface{})
		for _, childB := range childrenB {
			if childBMap, ok := childB.(map[string]interface{}); ok {
				if name, ok := childBMap["name"].(string); ok {
					childrenBMap[name] = childBMap
				}
			}
		}

		// 遍历A的children
		for _, childA := range childrenA {
			if childAMap, ok := childA.(map[string]interface{}); ok {
				if nameA, ok := childAMap["name"].(string); ok {
					if childBMap, exists := childrenBMap[nameA]; exists {
						compare(childAMap, childBMap, currentPath)
						delete(childrenBMap, nameA)
					} else {
						compare(childAMap, nil, currentPath)
					}
				}
			}
		}
	}

	// 开始比较
	compare(a, b, "")

	return diffs
}

// findDifferencesFromJSON 从JSON字符串比较差异
func FindDifferencesFromJSON(jsonA, jsonB string) ([]DiffResult, error) {
	var a, b map[string]interface{}

	if err := json.Unmarshal([]byte(jsonA), &a); err != nil {
		return nil, fmt.Errorf("解析JSON A失败: %v", err)
	}

	if err := json.Unmarshal([]byte(jsonB), &b); err != nil {
		return nil, fmt.Errorf("解析JSON B失败: %v", err)
	}

	return findDifferences(a, b), nil
}
